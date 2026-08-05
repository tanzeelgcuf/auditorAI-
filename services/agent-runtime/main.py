# services/agent-runtime/main.py
# Entry point — NATS consumer that runs the LangGraph pipeline per batch.

import asyncio
import json
import os
import signal
import structlog
from typing import Optional

from graph.schema import BookConfig
from graph.graph_def import build_graph

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
    try:
        pending = await mcp.get_pending_entities(client_book_id, batch_id=batch_id)
        tolerance = await mcp.get_book_tolerance(client_book_id)
    except Exception as e:
        logger.error("mcp fetch failed", error=str(e))
        return

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
    if groups:
        try:
            written = await mcp.persist_groups(groups, client_book_id)
            logger.info("groups persisted", client_book_id=client_book_id, written=written)
        except Exception as e:
            logger.error("group persistence failed", error=str(e))


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
    try:
        pending = await mcp.get_pending_entities(client_book_id)  # no batch — all unlinked
        tolerance = await mcp.get_book_tolerance(client_book_id)
    except Exception as e:
        logger.error("link mcp fetch failed", error=str(e))
        return

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
    try:
        entities = [ExtractedEntity(**e) for e in pending]
    except Exception as ex:
        logger.error("link: entity parse failed", error=str(ex))
        return
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
        try:
            written = await mcp.persist_groups(groups, client_book_id)
            logger.info("link groups persisted", client_book_id=client_book_id, written=written)
        except Exception as e:
            logger.error("link persistence failed", error=str(e))


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
                    await handler(client, graph, mcp, event)
                except Exception as e:
                    logger.error("batch processing failed", error=str(e))
                finally:
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
