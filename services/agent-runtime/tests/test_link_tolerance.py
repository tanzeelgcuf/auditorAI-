# services/agent-runtime/tests/test_link_tolerance.py
# CONFIDENCE IS NOT TOLERANCE.
#
# score_and_route's link_confidence answers "are these records the same
# transaction". Whether the amounts RECONCILE within the book's tolerance is a
# different question, and these tests pin the three places that conflated them:
#
#   1. is_exact compared every leg against present_totals[0] — a star, not all
#      pairs. Two legs each one tolerance off the invoice IN OPPOSITE DIRECTIONS
#      are 2x tolerance apart from each other and passed. services/verification
#      computes all three pairwise variances and takes the max (decimal_math/
#      mod.rs compute_three_way_variance + grpc/mod.rs:72), so the two tiers
#      disagreed and the group was published as reconciled while carrying an open
#      exceeds_tolerance finding.
#   2. Presence was `total != 0`, so a leg WITH members netting to zero (invoice
#      + its full credit note) was dropped from the comparison. The verification
#      tier decides presence from membership (verify_worker.go BOOL_OR(m.role=…)).
#      build_candidate_groups will not itself propose that membership, so this
#      one is reachable through the other group producers (mcp.go
#      HandleCreateEntityLink, humanoverride split/merge), not through matching.
#   3. _score_group's amount term used SIGNED subtraction, abs(vi - vj). Every
#      legitimate 3-way group carries opposite signs by convention, so a
#      near-miss group scored avg_variance ~= 1.33x max_amt, amount_score clamped
#      to 0.0, and the non-exact path collapsed to 0.2*date + 0.3*cp <= 0.5 —
#      landing on review_floor, where a slightly fuzzy counterparty dropped it
#      below and routed a real discrepancy to NEITHER queue.
#
# Per docs 06 §2 + 09 §1. No network, no API key needed.

import sys
import os
from datetime import date
from uuid import UUID

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, BookConfig, ReconciliationGroup
from graph.link import build_candidate_groups, score_and_route, _score_group

BOOK_ID = UUID("11111111-1111-1111-1111-111111111111")
DOC_ID = UUID("22222222-2222-2222-2222-222222222222")
CP = "Riverside Plumbing LLC"
DAY = date(2026, 3, 10)


def ent(etype, amount_cents, txn_date=DAY, counterparty=CP, subtype="standard"):
    return ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type=etype,
        entity_subtype=subtype,
        amount_cents=amount_cents,
        transaction_date=txn_date,
        counterparty=counterparty,
    )


def route(invoices, banks, gls, tolerance=1):
    config = BookConfig(id=BOOK_ID, tolerance_cents=tolerance)
    by_id = {str(e.id): e for e in invoices + banks + gls}
    candidates = build_candidate_groups(invoices, banks, gls, config)
    auto, review, unmatched = score_and_route(candidates, by_id, config)
    return auto, review, unmatched


def route_group(invoices, banks, gls, tolerance=1):
    """Route a group with a KNOWN membership, skipping candidate generation.

    build_candidate_groups selects invoice subsets that match the bank amount,
    so it will not hand score_and_route an invoice leg that nets to zero. Groups
    with arbitrary membership do reach score_and_route in production from other
    producers — mcp.go HandleCreateEntityLink takes invoice_ids/bank_ids/gl_ids
    straight from its caller, and humanoverride's split/merge rewrites
    reconciliation_group_members — so the presence rule has to hold for a
    membership the matcher itself would not have proposed.
    """
    config = BookConfig(id=BOOK_ID, tolerance_cents=tolerance)
    by_id = {str(e.id): e for e in invoices + banks + gls}
    group = ReconciliationGroup(
        client_book_id=BOOK_ID,
        invoice_entity_ids=[e.id for e in invoices],
        bank_entity_ids=[e.id for e in banks],
        gl_entity_ids=[e.id for e in gls],
    )
    return score_and_route([group], by_id, config)


# ---- 1. all-pairs, not a star ----

def test_opposite_spokes_do_not_auto_link():
    """The exact case the two tiers disagreed on. invoice +89900, bank -89899,
    GL +89901 at the default 1c tolerance: each leg is within tolerance of the
    INVOICE, but bank and GL are 2c apart. Rust's max pairwise variance is 2c,
    which the decision graph bands as low/exceeds_tolerance=true (pinned by
    zen/mod.rs test_one_cent_over_is_low)."""
    auto, review, _ = route(
        [ent("invoice_line_item", 89900)],
        [ent("bank_transaction", -89899)],
        [ent("gl_entry", 89901)],
        tolerance=1,
    )
    assert not auto, f"auto_linked a group whose bank and GL legs are 2c apart: {auto}"
    assert review, "the group must still reach a human, not vanish"


def test_opposite_spokes_scale_with_tolerance():
    """The window is 2x tolerance, so the absolute exposure grows with the book's
    tolerance. At tolerance=2500c this was $50.00 auto-linked at confidence 1.0."""
    for tol in (1, 5, 25, 100, 500, 2500):
        base = 500000
        auto, review, _ = route(
            [ent("invoice_line_item", base)],
            [ent("bank_transaction", -(base - tol))],
            [ent("gl_entry", base + tol)],
            tolerance=tol,
        )
        assert not auto, f"tolerance={tol}: auto_linked a group with a {2 * tol}c bank-GL gap"
        assert review, f"tolerance={tol}: group reached neither queue"


def test_all_legs_within_tolerance_still_auto_links():
    """The fix must not swallow genuinely reconciling groups: all three pairwise
    gaps at exactly tolerance is 'exact' (decimal_math compares <= tolerance)."""
    auto, review, _ = route(
        [ent("invoice_line_item", 500000)],
        [ent("bank_transaction", -500000)],
        [ent("gl_entry", 500100)],
        tolerance=100,
    )
    assert len(auto) == 1, f"a group inside tolerance on every pair must auto_link: {review}"
    assert auto[0].link_confidence == 1.0


def test_same_side_offsets_auto_link():
    """Both legs one tolerance off the invoice on the SAME side: bank-GL gap is
    0, so all three pairs are within tolerance. This is the case the star
    comparison got right, and it must stay right."""
    auto, _, _ = route(
        [ent("invoice_line_item", 500000)],
        [ent("bank_transaction", -500100)],
        [ent("gl_entry", 500100)],
        tolerance=100,
    )
    assert len(auto) == 1, "same-side offsets within tolerance must still auto_link"


# ---- 2. presence is membership, not a non-zero total ----

def test_zero_net_invoice_leg_is_compared_not_ignored():
    """An invoice and its full credit note net to 0. That leg HAS members, so
    verify_worker.go reports has_invoice=true (BOOL_OR over roles) and Rust
    compares 0 against the bank amount — a variance of the entire bank amount.
    Presence by `total != 0` made the leg invisible to is_exact, which then
    compared bank<->GL alone, found them equal, and auto_linked at 1.0.

    Measured against the pre-fix tree: auto=1 review=0 conf=1.0
    status='auto_linked'. Post-fix: auto=0 review=1 conf=0.6667
    status='needs_review'.

    Uses route_group, not route: see that helper for why the matcher itself
    cannot produce this membership and which producers can.
    """
    invoices = [
        ent("invoice_line_item", 50000),
        ent("invoice_line_item", -50000, subtype="credit_note"),
    ]
    auto, review, _ = route_group(
        invoices,
        [ent("bank_transaction", -89900)],
        [ent("gl_entry", 89900)],
        tolerance=1,
    )
    assert not auto, (
        "a group whose invoice leg has members but nets to 0 was auto_linked on "
        f"the strength of the other two legs: {auto}"
    )
    assert len(review) == 1, "the group must still reach a human, not vanish"
    assert review[0].link_confidence < 1.0


# ---- 3. the amount term compares magnitudes, and stays inside the queues ----

def test_score_group_amount_term_uses_magnitudes():
    """A 2c gap on an $899 invoice with opposite signs by convention. Signed
    subtraction gave abs(89900 - (-89899)) = 179799 and clamped amount_score to
    0.0; magnitudes give 1c/2c gaps and a score just under 1.0."""
    config = BookConfig(id=BOOK_ID, tolerance_cents=1)
    score = _score_group(
        [ent("invoice_line_item", 89900)],
        [ent("bank_transaction", -89899)],
        [ent("gl_entry", 89901)],
        config,
        is_exact=False,
    )
    assert score > 0.9, (
        f"a 2c gap on $899 scored {score}; signed subtraction is back and the "
        "non-exact path has collapsed to 0.2*date + 0.3*counterparty"
    )
    assert score < 1.0, "only is_exact returns exactly 1.0"


def test_near_miss_with_fuzzy_counterparty_still_reaches_review():
    """review_floor is 0.50 and the collapsed path scored 0.2*date + 0.3*cp. A
    counterparty at ~0.95 similarity put it at 0.485 — below the floor, so the
    group was routed to neither queue and the discrepancy disappeared instead of
    being reviewed."""
    auto, review, unmatched = route(
        [ent("invoice_line_item", 89900, counterparty="Riverside Plumbing LLC")],
        [ent("bank_transaction", -89899, counterparty="Riverside Plumbing")],
        [ent("gl_entry", 89901, counterparty="Riverside Plumbng LLC")],
        tolerance=1,
    )
    assert not auto, "a known over-tolerance group must not be auto_linked"
    assert review, (
        "a real over-tolerance discrepancy must reach the review queue; it "
        "scored below review_floor and was dropped"
    )


def test_exact_still_returns_one_point_zero():
    config = BookConfig(id=BOOK_ID, tolerance_cents=1)
    assert _score_group(
        [ent("invoice_line_item", 89900)],
        [ent("bank_transaction", -89900)],
        [ent("gl_entry", 89900)],
        config,
        is_exact=True,
    ) == 1.0
