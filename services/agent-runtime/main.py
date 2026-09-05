# services/agent-runtime/main.py
# Entry point — NATS consumer that runs the LangGraph pipeline per batch.

import asyncio
import json
import os
import signal
import structlog
import sentry_sdk
from typing import Optional

from graph.schema import BookConfig
from graph.graph_def import build_graph
from glitchtip_trace import init_glitchtip

logger = structlog.get_logger()

API_MCP_URL = os.getenv("API_MCP_URL", "http://api:8080")
ANTHROPIC_API_KEY = os.getenv("ANTHROPIC_API_KEY", "")
NATS_URL = os.getenv("NATS_URL", "nats://nats:4222")


async def process_batch(client, graph, mcp, batch_event: dict):
    """Handle one entity.extraction.requested event."""
    client_book_id = batch_event.get("client_book_id")
    batch_id = batch_event.get("batch_id")

    logger.info("processing batch", client_book_id=client_book_id, batch_id=batch_id)
    if not client_book_id or not batch_id:
        logger.error("batch event missing required fields")
        return

    # Fetch pending entities + book config via MCP. Scope to the batch's source
    # document — extraction is per-document, not per-book (a book may hold
    # hundreds of entities; a single LLM prompt must not span them all).
    #
    # No try/except here on purpose. This used to catch, log "mcp fetch failed"
    # and return — and because the caller acked unconditionally, an API restart or
    # a transient MCP error meant the batch was never extracted and nothing said
    # so. Letting it raise hands the decision to the consumer loop, which retries
    # a bounded number of times before giving up loudly.
    pending = await mcp.get_pending_entities(client_book_id, batch_id=batch_id)
    tolerance = await mcp.get_book_tolerance(client_book_id)

    config = BookConfig(
        id=client_book_id,
        tolerance_cents=int(tolerance.get("tolerance_cents", 1)),
        auto_link_threshold=float(tolerance.get("auto_link_threshold", 0.85)),
        review_floor=float(tolerance.get("review_floor", 0.50)),
    )

    state = {
        "client_book_id": client_book_id,
        "batch_id": batch_id,
        "book_config": config,
        "entities": pending,
    }

    result = await graph.arun(state)
    groups = result.get("groups", [])
    logger.info(
        "batch complete",
        client_book_id=client_book_id,
        batch_id=batch_id,
        auto_linked=len([g for g in groups if g.status == "auto_linked"]),
        needs_review=len([g for g in groups if g.status == "needs_review"]),
        errors=result.get("errors", []),
    )

    # Observability: emit a Langfuse span when configured (no-op otherwise).
    from langfuse_trace import trace_extraction
    trace_extraction(batch_id, client_book_id, len(result.get("classified_entities", [])))

    # Persist cross-linked groups back to the API (Prompt 3: the pipeline
    # dead-ended after extraction — groups were produced but never written).
    #
    # Allowed to raise. Swallowing this was the worst of the three swallows in
    # this file: the whole graph had already run, so the expensive work was done
    # and its ONLY output was this write. Catching the failure meant paying for
    # the extraction and discarding the result.
    if groups:
        written = await mcp.persist_groups(groups, client_book_id)
        logger.info("groups persisted", client_book_id=client_book_id, written=written)


async def process_link(client, graph, mcp, link_event: dict):
    """Handle one link.requested event — BOOK-WIDE linking.

    3-way reconciliation needs entities from the invoice + bank + GL documents
    together, which per-doc extraction can't provide. This fetches the whole
    book's pending entities and runs the link node across them.
    """
    client_book_id = link_event.get("client_book_id")
    if not client_book_id:
        logger.error("link event missing client_book_id")
        return

    logger.info("linking book", client_book_id=client_book_id)
    # Allowed to raise — see process_batch. This swallow was the worst of the
    # three in this file for a different reason than the persistence one: the
    # link pass is BOOK-WIDE, so one dropped link.requested does not leave a
    # single document unlinked, it leaves the entire book's three-way
    # reconciliation unrun with 'pending' entities and no finding anywhere.
    pending = await mcp.get_pending_entities(client_book_id)  # no batch — all unlinked
    tolerance = await mcp.get_book_tolerance(client_book_id)

    config = BookConfig(
        id=client_book_id,
        tolerance_cents=int(tolerance.get("tolerance_cents", 1)),
        auto_link_threshold=float(tolerance.get("auto_link_threshold", 0.85)),
        review_floor=float(tolerance.get("review_floor", 0.50)),
    )
    state = {
        "client_book_id": client_book_id,
        "batch_id": "book-wide-link",
        "book_config": config,
        "entities": pending,
    }
    # The MCP pending entities are already structured (ingestion parsed them);
    # re-running the LLM extract node would be redundant + slow on a whole book.
    # Convert to ExtractedEntity and run ONLY the deterministic link node.
    from graph.schema import ExtractedEntity
    from graph.link import cross_link
    # Also allowed to raise. A shape mismatch between extracted_entities and
    # ExtractedEntity is deterministic — it will fail identically on every
    # redelivery — so this burns the retry budget and then terms. That is still
    # the right trade against the previous behaviour: a silent return acked the
    # event, so a schema drift between the API's MCP payload and this model
    # would have stopped every book from linking with one ERROR line per book
    # and no other signal. Failing loudly after five attempts is louder.
    entities = [ExtractedEntity(**e) for e in pending]
    state["entities"] = entities
    state["classified_entities"] = entities  # cross_link reads this key
    linked = cross_link(state, config)
    groups = linked.get("groups", [])
    logger.info(
        "link complete",
        client_book_id=client_book_id,
        auto_linked=len([g for g in groups if g.status == "auto_linked"]),
        needs_review=len([g for g in groups if g.status == "needs_review"]),
        errors=linked.get("errors", []),
    )
    if groups:
        # Allowed to raise — see process_batch.
        written = await mcp.persist_groups(groups, client_book_id)
        logger.info("link groups persisted", client_book_id=client_book_id, written=written)


# Bounds redelivery for both consumers below. Matches maxDeliveryAttempts in
# services/api/internal/pipeline/coordinator.go so the two halves of the pipeline
# give up after the same number of tries.
MAX_DELIVERY_ATTEMPTS = 5


def _delivery_count(msg) -> int:
    """How many times JetStream has delivered this message (1 on first delivery).

    Defensive on purpose. nats-py exposes this as msg.metadata.num_delivered, but
    that attribute path could NOT be verified in the environment this was written
    in — nats-py is not installed there and PyPI was unreachable — so a mismatch
    must not take the consumer down. Falling back to 1 means a failing message is
    naked rather than dropped, which is the safe direction: at worst it retries
    more than intended, instead of losing the event the way the previous code did.
    """
    try:
        return int(msg.metadata.num_delivered)
    except Exception:  # noqa: BLE001 - any shape mismatch degrades, never raises
        logger.warning("could not read delivery count; retry bound not enforced")
        return 1


async def _retry_or_drop(msg, attempt: int) -> None:
    """Nak while attempts remain, otherwise stop redelivery.

    term() tells the server never to redeliver, which is what "give up" means
    here; ack() is the fallback if this nats-py build predates it, since both
    remove the message and only the bookkeeping differs.
    """
    if attempt < MAX_DELIVERY_ATTEMPTS:
        await msg.nak()
        return
    logger.error("event failed permanently, no further retries", attempt=attempt)
    term = getattr(msg, "term", None)
    if term is not None:
        await term()
    else:
        await msg.ack()


async def run_consumer():
    """Subscribe to NATS JetStream and process batches."""
    try:
        import nats
    except ImportError:
        logger.warning("nats-py not installed; running without consumer (dev mode)")
        # Keep alive so dev can attach a debugger
        while True:
            await asyncio.sleep(60)
        return

    nc = await nats.connect(NATS_URL)
    js = nc.jetstream()
    # The EXTRACTION/LINK streams are owned/created by services/api. Creating a
    # competing stream would fail with "subjects overlap"; bind consumers to the
    # existing streams instead (each is a WorkQueue with its single consumer).
    ext_sub = await js.subscribe("entity.extraction.requested")
    link_sub = await js.subscribe("link.requested")
    logger.info("nats consumer ready", url=NATS_URL)

    from ollama_adapter import make_llm_client
    client = make_llm_client()
    graph = build_graph(client)

    from mcp_client import MCPClient
    mcp = MCPClient(API_MCP_URL)

    async def consume(sub, handler):
        try:
            async for msg in sub.messages:
                try:
                    event = json.loads(msg.data.decode())
                except Exception as e:
                    # A malformed payload will never parse. Ack it — retrying is
                    # pointless and a nak would loop forever.
                    logger.error("undecodable event, dropping", error=str(e))
                    sentry_sdk.capture_exception(e)
                    await msg.ack()
                    continue

                try:
                    await handler(client, graph, mcp, event)
                except Exception as e:
                    # THIS USED TO BE `finally: await msg.ack()`, which acked on
                    # every path including this one. The LLM ran, the graph
                    # produced groups, the MCP write failed — and the event was
                    # acknowledged, so the work was lost permanently with nothing
                    # to retry it. The document's entities simply never got linked
                    # and no reconciliation ever referenced them. Same bug class as
                    # the two Go consumers in services/api/internal/pipeline.
                    attempt = _delivery_count(msg)
                    logger.error(
                        "batch processing failed",
                        error=str(e),
                        attempt=attempt,
                        max_attempts=MAX_DELIVERY_ATTEMPTS,
                    )
                    sentry_sdk.capture_exception(e)  # GlitchTip (no-op when DSN unset)
                    await _retry_or_drop(msg, attempt)
                else:
                    await msg.ack()
        finally:
            await nc.drain()

    await asyncio.gather(
        consume(ext_sub, process_batch),
        consume(link_sub, process_link),
    )


def setup_structlog():
    structlog.configure(
        processors=[
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso"),
            structlog.processors.StackInfoRenderer(),
            structlog.processors.format_exc_info,
            structlog.dev.ConsoleRenderer(),
        ],
        context_class=dict,
        cache_logger_on_first_use=True,
    )


async def main():
    setup_structlog()
    init_glitchtip()  # error reporting to GlitchTip (no-op when GLITCHTIP_DSN unset)
    logger.info("starting agent-runtime service")

    stop = asyncio.Event()

    def _on_signal():
        logger.info("shutdown signal received")
        stop.set()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, _on_signal)

    consumer = asyncio.create_task(run_consumer())
    await stop.wait()
    consumer.cancel()
    logger.info("agent-runtime stopped")


if __name__ == "__main__":
    asyncio.run(main())
