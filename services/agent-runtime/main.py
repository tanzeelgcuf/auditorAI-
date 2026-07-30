# services/agent-runtime/main.py
# Entry point — starts LangGraph agent service

import asyncio
import os
import structlog
from anthropic import Anthropic
from langfuse import Langfuse

from .graph import build_graph

logger = structlog.get_logger()


async def main():
    logger.info("starting agent-runtime service")

    # Initialize clients
    api_key = os.getenv("ANTHROPIC_API_KEY")
    if not api_key:
        logger.error("ANTHROPIC_API_KEY not set")
        return

    client = Anthropic(api_key=api_key)

    # Optional: Langfuse for tracing
    if os.getenv("LANGFUSE_PUBLIC_KEY") and os.getenv("LANGFUSE_SECRET_KEY"):
        langfuse = Langfuse(
            public_key=os.getenv("LANGFUSE_PUBLIC_KEY"),
            secret_key=os.getenv("LANGFUSE_SECRET_KEY"),
            host=os.getenv("LANGFUSE_HOST", "http://langfuse:3000"),
        )
        logger.info("langfuse tracing enabled")
    else:
        logger.warning("langfuse not configured, tracing disabled")

    # Build LangGraph pipeline
    pipeline = await build_graph(client)
    logger.info("pipeline ready")

    # TODO: Start message consumer (NATS JetStream) to process document batches
    # and keep the process alive
    try:
        while True:
            await asyncio.sleep(60)
    except KeyboardInterrupt:
        logger.info("shutting down")


if __name__ == "__main__":
    structlog.configure(
        processors=[
            structlog.stdlib.filter_by_level,
            structlog.stdlib.add_logger_name,
            structlog.stdlib.add_log_level,
            structlog.processors.TimeStamper(fmt="iso"),
            structlog.dev.ConsoleRenderer(),
        ],
        wrapper_class=structlog.stdlib.BoundLogger,
        context_class=dict,
        logger_factory=structlog.PrintLoggerFactory(),
        cache_logger_on_first_use=True,
    )

    asyncio.run(main())