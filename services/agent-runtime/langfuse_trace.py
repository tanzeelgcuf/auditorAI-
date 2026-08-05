"""Thin Langfuse trace emission for the extraction pipeline.

The langfuse dep was declared but never used (Part 3 observability FAIL). This
module emits a span per extraction batch when LANGFUSE_PUBLIC_KEY/SECRET are
set (Langfuse reachable); otherwise it's a no-op so local/dev runs don't
depend on a Langfuse server. Kept minimal — no langchain callback dependency.
"""
import os
import structlog

logger = structlog.get_logger()

_USE_LANGFUSE = bool(os.getenv("LANGFUSE_PUBLIC_KEY") and os.getenv("LANGFUSE_SECRET_KEY"))


def trace_extraction(batch_id: str, client_book_id: str, entity_count: int):
    """Record one extraction batch as a Langfuse span (no-op if not configured)."""
    if not _USE_LANGFUSE:
        return
    try:
        from langfuse import Langfuse

        lf = Langfuse()
        span = lf.start_observation(
            name="extraction",
            as_type="span",
            input={"batch_id": batch_id, "client_book_id": client_book_id},
        )
        span.update(output={"entity_count": entity_count})
        span.end()
        lf.flush()
    except Exception as e:
        logger.warning("langfuse trace failed", error=str(e))
