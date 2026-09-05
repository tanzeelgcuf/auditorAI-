# services/agent-runtime/tests/test_verify_node.py
# _verify_node OBTAINS a deterministic verdict and must APPLY it. Three defects
# lived in it, all of which produced a group published as reconciled that nothing
# had actually verified:
#
#   1. has_invoice/has_bank/has_gl were never sent. proto3 defaults an unset bool
#      to false and grpc/mod.rs:130-141 builds each leg only `if req.has_X`, so
#      every call arrived with all three legs ABSENT: three empty slices into
#      compute_three_way_variance, no variances, grpc/mod.rs:72 folds to max 0,
#      and the decision graph bands 0 as info / exceeds_tolerance=false. The
#      answer was fixed at "clean" before the group was looked at.
#   2. The result went into state["results"] (which nothing reads) and the
#      group's status was left as the matcher set it.
#   3. The except branch logged and moved on, leaving the group auto_linked —
#      verification failing is precisely when a group must not read as clean.
#
# These tests use a recording fake for the gRPC client: what is asserted is the
# CALL (that presence flags and totals are sent) and the EFFECT (what happens to
# group.status), never Rust's arithmetic, which is not this tier's to reproduce.
#
# NOTE ON REACHABILITY, so these do not read as more than they are: main.py:212
# calls build_graph(client) with no verification_client, so this node does not
# exist in the deployed graph. The authoritative downgrade is
# services/api/internal/pipeline/verify_worker.go. These tests pin the node
# against the day it is wired in, and pin that its contract matches the Go
# worker's (presence by membership, downgrade only, fail closed).

import sys
import os
from datetime import date
from uuid import UUID, uuid4

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, BookConfig, ReconciliationGroup
from graph.graph_def import _verify_node

BOOK_ID = UUID("11111111-1111-1111-1111-111111111111")
DOC_ID = UUID("22222222-2222-2222-2222-222222222222")
DAY = date(2026, 3, 10)


def ent(etype, amount_cents, subtype="standard"):
    return ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type=etype,
        entity_subtype=subtype,
        amount_cents=amount_cents,
        transaction_date=DAY,
        counterparty="Riverside Plumbing LLC",
    )


class Result:
    """Stands in for schema.ReconciliationResult / the proto message. Only the
    fields _verify_node reads are present, so a rename there fails loudly here
    instead of being silently absorbed by getattr's default."""

    def __init__(self, exceeds_tolerance, variance_cents=0, severity="info"):
        self.exceeds_tolerance = exceeds_tolerance
        self.variance_cents = variance_cents
        self.severity = severity


class FakeClient:
    """Records every evaluate_reconciliation kwargs dict; returns queued results
    in order (or raises, if a queued item is an exception)."""

    def __init__(self, *results):
        self.results = list(results)
        self.calls = []

    def evaluate_reconciliation(self, **kwargs):
        self.calls.append(kwargs)
        r = self.results.pop(0) if self.results else Result(False)
        if isinstance(r, Exception):
            raise r
        return r


def group(invoices, banks, gls, status="auto_linked"):
    return ReconciliationGroup(
        client_book_id=BOOK_ID,
        invoice_entity_ids=[e.id for e in invoices],
        bank_entity_ids=[e.id for e in banks],
        gl_entity_ids=[e.id for e in gls],
        status=status,
        link_confidence=1.0,
    )


def state_for(groups, entities, tolerance=1):
    return {
        "client_book_id": BOOK_ID,
        "book_config": BookConfig(id=BOOK_ID, tolerance_cents=tolerance),
        "classified_entities": list(entities),
        "groups": list(groups),
    }


# ---- 1. the presence flags reach the wire ----

def test_presence_flags_are_sent():
    """Defect 1. Without these three kwargs the Rust side sees no legs at all and
    cannot return exceeds_tolerance=true for any input."""
    inv, bank, gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([inv], [bank], [gl])
    client = FakeClient(Result(False))
    _verify_node(state_for([g], [inv, bank, gl]), client)

    assert len(client.calls) == 1, f"expected one gRPC call, got {len(client.calls)}"
    call = client.calls[0]
    for flag in ("has_invoice", "has_bank", "has_gl"):
        assert flag in call, (
            f"{flag} was not sent; proto3 will default it to false and "
            "grpc/mod.rs will build an empty leg"
        )
        assert call[flag] is True, f"{flag} sent as {call[flag]!r} for a leg with members"
    assert call["tolerance_cents"] == 1
    assert call["client_book_id"] == str(BOOK_ID)


def test_absent_leg_reports_false_not_a_zero_total():
    """A two-leg group (bank+GL only — deposits, fees) must report
    has_invoice=false, NOT has_invoice=true with a total of 0. Those two are
    different questions to the money tier: absent means 'do not compare this
    leg', zero means 'this leg says zero'."""
    bank, gl = ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([], [bank], [gl])
    client = FakeClient(Result(False))
    _verify_node(state_for([g], [bank, gl]), client)

    call = client.calls[0]
    assert call["has_invoice"] is False, "a leg with no members must report absent"
    assert call["has_bank"] is True and call["has_gl"] is True
    assert call["invoice_amount_cents"] == 0


def test_zero_net_leg_is_present_with_total_zero():
    """The other half of the same rule, and the one that diverged from
    verify_worker.go's BOOL_OR(m.role=…): an invoice plus its full credit note
    nets to 0, but the leg HAS members, so it is present with total 0 and Rust
    compares 0 against the bank amount."""
    inv_a = ent("invoice_line_item", 50000)
    inv_b = ent("invoice_line_item", -50000, subtype="credit_note")
    bank, gl = ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([inv_a, inv_b], [bank], [gl])
    client = FakeClient(Result(True, variance_cents=89900, severity="high"))
    _verify_node(state_for([g], [inv_a, inv_b, bank, gl]), client)

    call = client.calls[0]
    assert call["has_invoice"] is True, (
        "a leg whose members net to zero is present with total 0, not absent"
    )
    assert call["invoice_amount_cents"] == 0
    assert g.status == "needs_review"


def test_totals_are_a_plain_sum_of_extracted_cents():
    """The node may sum already-extracted integer cents to fill the request; it
    must not compute a variance, a percentage, or a verdict. Asserted here as
    the literal expected sums so a stray calculation shows up as a diff."""
    inv = [ent("invoice_line_item", 60000), ent("invoice_line_item", 29900)]
    bank = [ent("bank_transaction", -89900)]
    gl = [ent("gl_entry", 89901)]
    g = group(inv, bank, gl)
    client = FakeClient(Result(True, variance_cents=1, severity="low"))
    _verify_node(state_for([g], inv + bank + gl), client)

    call = client.calls[0]
    assert call["invoice_amount_cents"] == 89900
    assert call["bank_amount_cents"] == -89900
    assert call["gl_amount_cents"] == 89901


# ---- 2. the verdict is applied ----

def test_exceeds_tolerance_downgrades_to_needs_review():
    """Defect 2. The result used to land in state['results'], which nothing
    reads, while status stayed auto_linked and main.py persisted it."""
    inv, bank, gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89899), ent("gl_entry", 89901)
    g = group([inv], [bank], [gl])
    client = FakeClient(Result(True, variance_cents=2, severity="low"))
    _verify_node(state_for([g], [inv, bank, gl]), client)

    assert g.status == "needs_review", (
        f"verification said exceeds_tolerance=True and the group is still {g.status!r}"
    )


def test_clean_verdict_leaves_auto_linked_alone():
    """The fix must not invert into flagging everything."""
    inv, bank, gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([inv], [bank], [gl])
    client = FakeClient(Result(False))
    _verify_node(state_for([g], [inv, bank, gl]), client)

    assert g.status == "auto_linked"


def test_needs_review_group_is_never_promoted():
    """Downgrade only, never promote — same rule as verify_worker.go's
    `AND status = 'auto_linked'` guard. exceeds_tolerance=false answers 'the
    amounts reconcile', not 'a human need not look': a group is in review for
    reasons the deterministic tier cannot see (fuzzy counterparty, date drift).
    The node must not even call out for a group it has no power to change."""
    inv, bank, gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([inv], [bank], [gl], status="needs_review")
    client = FakeClient(Result(False))
    _verify_node(state_for([g], [inv, bank, gl]), client)

    assert g.status == "needs_review", f"a review group was promoted to {g.status!r}"
    assert client.calls == [], "a group that cannot be downgraded should not be verified"


# ---- 3. failure paths fail closed ----

def test_grpc_exception_fails_closed():
    """Defect 3. A group whose verification call blew up is a group nothing
    verified; leaving it auto_linked publishes it as reconciled."""
    inv, bank, gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([inv], [bank], [gl])
    client = FakeClient(RuntimeError("verification unavailable"))
    state = state_for([g], [inv, bank, gl])
    _verify_node(state, client)

    assert g.status == "needs_review", (
        f"verification raised and the group is still {g.status!r}"
    )
    assert any("Verification error" in e for e in state.get("errors", [])), (
        f"the failure was not recorded on state['errors']: {state.get('errors')}"
    )


def test_unresolved_member_fails_closed_without_calling_out():
    """A member id with no matching classified entity would silently shrink that
    leg's total, and the money tier's answer would then be precise and wrong.
    Fail closed instead of verifying a total known to be incomplete."""
    inv, bank, gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89900), ent("gl_entry", 89900)
    g = group([inv], [bank], [gl])
    g.invoice_entity_ids.append(uuid4())  # a member that was never classified
    client = FakeClient(Result(False))
    state = state_for([g], [inv, bank, gl])
    _verify_node(state, client)

    assert g.status == "needs_review", (
        f"a group with an unresolved member is still {g.status!r}"
    )
    assert client.calls == [], (
        "an incomplete total must not be sent to the money tier: "
        f"{client.calls}"
    )
    assert any("Unresolved group members" in e for e in state.get("errors", []))


def test_one_bad_group_does_not_stop_the_others():
    """Per-group failure isolation: the first group raises, the second must still
    be verified and downgraded."""
    a_inv, a_bank, a_gl = ent("invoice_line_item", 10000), ent("bank_transaction", -10000), ent("gl_entry", 10000)
    b_inv, b_bank, b_gl = ent("invoice_line_item", 89900), ent("bank_transaction", -89899), ent("gl_entry", 89901)
    ga, gb = group([a_inv], [a_bank], [a_gl]), group([b_inv], [b_bank], [b_gl])
    client = FakeClient(RuntimeError("transient"), Result(True, variance_cents=2, severity="low"))
    state = state_for([ga, gb], [a_inv, a_bank, a_gl, b_inv, b_bank, b_gl])
    _verify_node(state, client)

    assert ga.status == "needs_review" and gb.status == "needs_review"
    assert len(client.calls) == 2, f"the second group was not verified: {client.calls}"
