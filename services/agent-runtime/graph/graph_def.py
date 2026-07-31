# services/agent-runtime/graph/graph_def.py
# LangGraph StateGraph wiring: extract -> classify -> link -> (conditional) verify/review

import structlog
from typing import Literal, TYPE_CHECKING, Any

if TYPE_CHECKING:
    from anthropic import Anthropic

from .schema import GraphState
from .extract import extract_entities, classify_entities
from .link import cross_link

logger = structlog.get_logger()


def build_graph(client: Any, verification_client=None):
    """Build the LangGraph processing pipeline.

    extract -> classify -> link -> (conditional) verify | human-review

    verification_client: optional gRPC client to services/verification.
    The verify node is added only if a verification_client is provided;
    otherwise routing goes straight to human review (safe default).
    """
    try:
        from langgraph.graph import StateGraph, END
    except ImportError:
        # Fallback: sequential pipeline when langgraph is unavailable (tests)
        return _SequentialPipeline(client, verification_client)

    workflow = StateGraph(GraphState)

    # Nodes
    workflow.add_node("extract", lambda state: extract_entities(state, client))
    workflow.add_node("classify", lambda state: classify_entities(state, client))
    workflow.add_node("link", lambda state: cross_link(state, state.get("book_config") or _default_config(state)))
    if verification_client is not None:
        workflow.add_node("verify", lambda state: _verify_node(state, verification_client))

    # Edges
    workflow.set_entry_point("extract")
    workflow.add_edge("extract", "classify")
    workflow.add_edge("classify", "link")

    def route_after_link(state: GraphState) -> Literal["verify", "review_human", "__end__"]:
        if state.get("errors"):
            return "review_human"
        groups = state.get("groups") or []
        if verification_client is None:
            return "review_human"
        if not groups:
            return "review_human"
        return "verify"

    if verification_client is not None:
        workflow.add_conditional_edges("link", route_after_link, {
            "verify": "verify",
            "review_human": END,
        })
        workflow.add_edge("verify", END)
    else:
        workflow.add_conditional_edges("link", route_after_link, {
            "review_human": END,
        })

    return workflow.compile()


def _default_config(state: GraphState):
    from .schema import BookConfig
    return BookConfig(id=state.get("client_book_id"))


def _verify_node(state: GraphState, verification_client):
    """Call services/verification over gRPC for each auto-linked group.

    The agent NEVER computes variance itself — it sends the group totals and
    receives the deterministic result back. This enforces the LLM boundary.
    """
    import structlog
    logger = structlog.get_logger()
    groups = state.get("groups") or []
    results = []
    for group in groups:
        if group.status != "auto_linked":
            continue
        try:
            # totals computed in this node are just SUM of cents for the gRPC call —
            # the actual variance/severity is decided entirely by verification.
            ids = {str(i) for i in group.invoice_entity_ids}
            invoice_total = sum(
                e.amount_cents for e in state.get("classified_entities", [])
                if str(e.id) in ids
            )
            ids = {str(i) for i in group.bank_entity_ids}
            bank_total = sum(
                e.amount_cents for e in state.get("classified_entities", [])
                if str(e.id) in ids
            )
            ids = {str(i) for i in group.gl_entity_ids}
            gl_total = sum(
                e.amount_cents for e in state.get("classified_entities", [])
                if str(e.id) in ids
            )
            tolerance = (state.get("book_config") or _default_config(state)).tolerance_cents

            result = verification_client.evaluate_reconciliation(
                client_book_id=str(group.client_book_id),
                invoice_amount_cents=invoice_total,
                bank_amount_cents=bank_total,
                gl_amount_cents=gl_total,
                tolerance_cents=tolerance,
            )
            results.append(result)
        except Exception as e:
            logger.error("verification call failed", group_id=str(group.id), error=str(e))
            state.setdefault("errors", []).append(f"Verification error: {e}")

    state["results"] = results
    return state


class _SequentialPipeline:
    """Fallback sequential pipeline (no langgraph dependency) for tests/CI."""

    def __init__(self, client, verification_client=None):
        self.client = client
        self.verification_client = verification_client

    def run(self, state: GraphState) -> GraphState:
        state = extract_entities(state, self.client)
        if state.get("errors"):
            return state
        state = classify_entities(state, self.client)
        if state.get("errors"):
            return state
        config = state.get("book_config") or _default_config(state)
        state = cross_link(state, config)
        return state

    async def arun(self, state: GraphState) -> GraphState:
        return self.run(state)
