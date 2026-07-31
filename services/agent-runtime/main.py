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

    # Fetch pending entities + book config via MCP
    try:
        pending = await mcp.get_pending_entities(client_book_id)
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
    logger.info(
        "batch complete",
        client_book_id=client_book_id,
        batch_id=batch_id,
        auto_linked=len([g for g in result.get("groups", []) if g.status == "auto_linked"]),
        needs_review=len([g for g in result.get("groups", []) if g.status == "needs_review"]),
        errors=result.get("errors", []),
    )


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
    await js.add_stream(name="ENTITY_EXTRACTION", subjects=["entity.extraction.requested"])

    sub = await js.subscribe("entity.extraction.requested")
    logger.info("nats consumer ready", url=NATS_URL)

    from anthropic import Anthropic
    client = Anthropic(api_key=ANTHROPIC_API_KEY) if ANTHROPIC_API_KEY else None
    graph = build_graph(client)

    from mcp_client import MCPClient
    mcp = MCPClient(API_MCP_URL)

    try:
        async for msg in sub.messages:
            try:
                event = json.loads(msg.data.decode())
                await process_batch(client, graph, mcp, event)
            except Exception as e:
                logger.error("batch processing failed", error=str(e))
            finally:
                await msg.ack()
    finally:
        await nc.drain()


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
