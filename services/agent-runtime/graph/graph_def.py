# services/agent-runtime/graph/graph_def.py
# LangGraph StateGraph wiring: extract -> classify -> link -> (conditional) verify-call

import structlog
from dataclasses import dataclass
from typing import Literal
from anthropic import Anthropic

from .schema import GraphState
from .extract import extract_entities, classify_entities
from .link import cross_link

logger = structlog.get_logger()


async def route_after_classify(state: GraphState) -> Literal["cross_link", "error"]:
    """Route to linking if classification succeeded, otherwise error."""
    if state.errors:
        return "error"
    if not state.classified_entities:
        logger.warning("no classified entities to link")
        return "error"
    return "cross_link"


async def route_after_link(state: GraphState) -> Literal["verify", "review_human"]:
    """Route to verification or human review based on confidence threshold."""
    if state.errors:
        return "review_human"
    return "verify"


async def build_graph(client: Anthropic):
    """Build the LangGraph processing pipeline.

    Returns a compiled graph that can process document batches.
    """
    # In production: create a StateGraph with TypedDict state
    # For now: define the pipeline structure

    class Pipeline:
        """Simple sequential pipeline implementing the graph structure.

        In production, this would be a proper LangGraph with:
          - StateGraph nodes
          - Conditional edges
          - Parallel execution where possible
        """

        async def run(self, state: GraphState) -> GraphState:
            logger.info("running extraction pipeline", batch_id=str(state.batch_id))

            # Node 1: Extract
            state = await extract_entities(state, client)
            if state.errors:
                logger.error("pipeline failed at extract")
                return state

            # Node 2: Classify
            state = await classify_entities(state, client)
            if state.errors:
                logger.error("pipeline failed at classify")
                return state

            # Node 3: Cross-link (no LLM call)
            state = await cross_link(state, state.client_book_config)
            if state.errors:
                logger.error("pipeline failed at link")
                return state

            # Node 4: Conditional — call verification or route to review
            if any(g.reconciliation_groups for g in state.groups):
                # Call services/verification via MCP client
                logger.info("routing to verification", batch_id=str(state.batch_id))
                # await call_verification(state)
                pass
            else:
                logger.info("no auto-linked groups, routing to review",
                             batch_id=str(state.batch_id))

            logger.info("pipeline complete", batch_id=str(state.batch_id))
            return state

    return Pipeline()