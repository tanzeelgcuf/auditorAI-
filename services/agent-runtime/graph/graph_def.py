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

    extract -> classify -> link -> (conditional) verify | end

    verification_client: optional gRPC client to services/verification. The
    verify node is added only if one is provided.

    NOTE ON THE "review_human" BRANCH, which used to be described here as a
    "safe default": it is not a default and it is not safe. "review_human" maps
    to END (see add_conditional_edges below), so taking that branch runs no
    review step and changes no status — main.py persists the groups exactly as
    the matcher routed them, auto_linked ones included. It is simply "stop here".
    Nothing in the graph sends a group to a human.

    main.py:212 calls build_graph(client) with NO verification_client, so in the
    deployed pipeline the verify node does not exist and every batch takes that
    branch. The deterministic verdict is applied by the NATS consumer instead
    (services/api/internal/pipeline/verify_worker.go), which is the authority on
    disposition and the only writer of audit_findings.
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
    """Call services/verification over gRPC for each auto-linked group and APPLY
    the verdict to the group's status.

    The agent NEVER computes variance itself — it sends the group totals and
    receives the deterministic result back. This enforces the LLM boundary.

    Three defects lived here, all the same class as verify_worker.go's: the
    deterministic verdict was obtained and then discarded.

    1. THE PRESENCE FLAGS WERE NEVER SENT. proto/verification.proto's
       ReconciliationRequest carries has_invoice/has_bank/has_gl (fields 6-8) and
       grpc/mod.rs:130-141 builds each leg only `if req.has_X`. proto3 defaults an
       unset bool to false, so every call made from here arrived with all three
       legs ABSENT: compute_three_way_variance received three empty slices and
       returned no variances, grpc/mod.rs:72 folded that to max = 0, and the
       decision graph bands 0 as info / exceeds_tolerance = false. This node could
       not report a discrepancy for ANY group — the answer was structurally fixed
       at "clean" before the group was even looked at. (Rust's own
       test_three_way_empty_groups pins the empty-slice case at [].)

    2. THE RESULT WAS NEVER APPLIED. `results` went into state["results"], which
       nothing reads, and group.status was left as the matcher set it. main.py
       then persists that status through mcp.persist_groups, so a group the
       verification tier disagreed with was written as auto_linked.

    3. THE EXCEPTION PATH LEFT THE GROUP auto_linked. Verification failing is
       exactly when a group must not read as reconciled. It now fails closed.

    This APPLIES the verdict; it does not compute it and it is not the authority
    on it. services/api/internal/pipeline/verify_worker.go re-evaluates the group
    once it is persisted, writes audit_findings, and performs the authoritative
    downgrade. This node exists so that a group is not created auto_linked in the
    first place when the deterministic tier already disagrees. Downgrade only,
    never promote — same rule and same reasons as verify_worker.go.
    """
    logger = structlog.get_logger()
    groups = state.get("groups") or []
    results = []
    entities_by_id = {
        str(e.id): e for e in (state.get("classified_entities") or [])
    }
    for group in groups:
        if group.status != "auto_linked":
            continue
        try:
            # Totals here are a plain SUM of already-extracted cents for the gRPC
            # call — the variance, severity and verdict are decided entirely by
            # services/verification.
            def leg(entity_ids):
                """(total_cents, present, all_members_resolved).

                Presence is MEMBERSHIP, matching verify_worker.go's
                BOOL_OR(m.role=…) and score_and_route's is_exact: a leg whose
                amounts net to zero is present with total 0, not absent.
                """
                ids = [str(i) for i in entity_ids]
                found = [entities_by_id[i] for i in ids if i in entities_by_id]
                return (sum(e.amount_cents for e in found),
                        len(ids) > 0,
                        len(found) == len(ids))

            invoice_total, has_invoice, inv_ok = leg(group.invoice_entity_ids)
            bank_total, has_bank, bank_ok = leg(group.bank_entity_ids)
            gl_total, has_gl, gl_ok = leg(group.gl_entity_ids)

            # A member id that does not resolve to a classified entity would
            # silently shrink that leg's total and send a WRONG NUMBER to the
            # money tier, whose answer would then be precise and meaningless.
            # Fail closed rather than verify a total we know is incomplete.
            if not (inv_ok and bank_ok and gl_ok):
                logger.error("group has unresolved members, not verifying",
                             group_id=str(group.id))
                group.status = "needs_review"
                state.setdefault("errors", []).append(
                    f"Unresolved group members: {group.id}")
                continue

            tolerance = (state.get("book_config") or _default_config(state)).tolerance_cents

            result = verification_client.evaluate_reconciliation(
                client_book_id=str(group.client_book_id),
                invoice_amount_cents=invoice_total,
                bank_amount_cents=bank_total,
                gl_amount_cents=gl_total,
                tolerance_cents=tolerance,
                has_invoice=has_invoice,
                has_bank=has_bank,
                has_gl=has_gl,
            )
            results.append(result)

            # schema.ReconciliationResult.exceeds_tolerance / proto field 2.
            if getattr(result, "exceeds_tolerance", False):
                group.status = "needs_review"
                logger.info("group downgraded by verification",
                            group_id=str(group.id),
                            variance_cents=getattr(result, "variance_cents", None),
                            severity=getattr(result, "severity", None))
        except Exception as e:
            # Fail closed. This used to log and move on, leaving the group
            # auto_linked — a group nothing verified, published as reconciled.
            logger.error("verification call failed", group_id=str(group.id), error=str(e))
            group.status = "needs_review"
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
