# services/agent-runtime/tests/test_link_sign.py
# MAGNITUDE IS NOT SIDE.
#
# _amounts_match compares abs(), by a contract it shares with the Rust verifier
# (compute_three_way_variance takes .abs() on all three pairs and names
# _amounts_match as the reason). One consequence had no test: a leg of the right
# SIZE on the wrong SIDE is variance 0, so is_exact is True, confidence is 1.0,
# and the group auto-links with nobody looking. At the repo fixtures' own sign
# convention that is invoice +150000 / bank +150000 / GL -150000 published as a
# clean reconciliation, when it is a wrong-side posting or a refund matched to
# the charge it reverses.
#
# WHAT THIS SUITE IS CAREFUL ABOUT, and it is the whole reason the gate is as
# narrow as it is. This repo holds TWO sign conventions and both are exercised by
# passing tests:
#
#   - tests/fixtures/* (the only data here from real file formats — CSV invoice,
#     OFX bank, CSV GL) parse to legs that are sign-for-sign IDENTICAL. The
#     fixture builder aligns them on purpose: "OFX expresses the refund as a
#     positive CREDIT while the invoice/GL express it as a negative refund.
#     Align to the invoice sign convention." So sign encodes charge-vs-refund.
#   - _score_group's own comment and decimal_math/mod.rs's own comment both say
#     the opposite: "a billed invoice +, its bank debit −, its GL credit +", and
#     tests/test_link_tolerance.py hand-builds exactly that.
#
# Measured 2026-09-06: a gate enforcing the second (invoice↔bank opposite) fails
# 10 of 77 tests, including the real-fixture one. So the gate asserts only the
# single pattern impossible under EVERY convention the codebase exhibits — if
# invoice and bank agree in sign, GL must agree too — and asserts nothing at all
# when invoice and bank disagree, on 2-leg groups, or on a zero-netting leg.
#
# Downgrade-only, per rule 9: it can move auto_linked → needs_review and can
# never suppress a candidate or raise a claim.
#
# NON-VACUITY, measured 2026-09-06 against `git archive HEAD` (the tree before
# the gate existed), not asserted. 3 passed / 8 failed, and the attribution is
# the point rather than the count:
#
#   RIGHT-REASON behavioural failures (the bug, reproduced):
#     gl_alone_on_the_other_side_downgrades_to_review
#       AssertionError: auto_linked a group whose GL leg is on the opposite side
#       from both the invoice and the bank: [(1.0, 'auto_linked')]
#     downgrade_holds_when_all_three_are_negative_but_gl_is_positive
#     sign_conflict_reaches_review_even_with_no_date_or_counterparty_signal
#   INCIDENTAL failures — the guards' behavioural assertion (len(auto) == 1)
#   passed pre-fix and they died on the provenance field not existing yet:
#     all_three_same_sign / invoice_bank_opposite_gl_with_invoice /
#     invoice_bank_opposite_gl_with_bank / two_leg_bank_gl
#       AttributeError: 'ReconciliationGroup' object has no attribute 'sign_conflict'
#     zero_netting_invoice_leg  ImportError: cannot import _sign_pattern_ok
#   PASSED ON BOTH TREES, all three declared guards, which is the intended
#   result — a non-guard in this list would be the vacuous-test failure mode:
#     amounts_match_stays_magnitude_only / fixtures_are_still_sign_aligned /
#     rust_verifier_still_compares_absolute_values
#
# _sign_pattern_ok is imported lazily below for the same reason: a module-level
# import of it collapses all eleven results into one IMPORT ERROR on the pre-fix
# tree and destroys this reading.
#
# No network, no API key, no database. Run under the stdlib pyshim, so pydantic,
# structlog and jellyfish are shims — see pyshim/*.py for what that does and
# does not prove.

import os
import re
import sys
from datetime import date
from uuid import UUID

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, BookConfig, ReconciliationGroup
from graph.link import score_and_route, _amounts_match

BOOK_ID = UUID("11111111-1111-1111-1111-111111111111")
DOC_ID = UUID("22222222-2222-2222-2222-222222222222")
CP = "Riverside Plumbing LLC"
DAY = date(2026, 3, 10)
REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


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


def route_group(invoices, banks, gls, tolerance=1):
    """Route a KNOWN membership, skipping candidate generation.

    build_candidate_groups will not propose a wrong-side group, because its own
    amount gates run on abs() and its date/counterparty gates are indifferent to
    sign — so it would propose one only from real data. Groups with arbitrary
    membership reach score_and_route in production from the other producers
    (mcp.go HandleCreateEntityLink takes invoice_ids/bank_ids/gl_ids straight
    from its caller; humanoverride split/merge rewrites the members), which is
    where a wrong-side group actually arrives. Same helper and same reasoning as
    test_link_tolerance.route_group.
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


# ---- 1. the defect: wrong-side GL leg auto-linked at confidence 1.0 ----

def test_gl_alone_on_the_other_side_downgrades_to_review():
    """The case that was published as clean. Invoice and bank agree (the
    fixtures' convention), GL is the odd one out. Every pairwise abs() variance
    is 0, so this scored is_exact True and confidence 1.0."""
    auto, review, unmatched = route_group(
        [ent("invoice_line_item", 150000)],
        [ent("bank_transaction", 150000)],
        [ent("gl_entry", -150000)],
    )
    assert not auto, (
        "auto_linked a group whose GL leg is on the opposite side from both the "
        "invoice and the bank: %s" % [(g.link_confidence, g.status) for g in auto])
    assert len(review) == 1, "the group must reach a human, not vanish: %r" % (unmatched,)
    assert review[0].status == "needs_review"
    assert review[0].sign_conflict is True, "provenance must say WHY it was downgraded"
    assert not unmatched, "downgrading must not orphan the entities"


def test_downgrade_holds_when_all_three_are_negative_but_gl_is_positive():
    """The mirror image. The rule is about the pattern, not about which sign is
    'the' positive one — a global flip preserves every pairwise relation, so
    flipping the whole group must give the same verdict."""
    auto, review, _ = route_group(
        [ent("invoice_line_item", -150000)],
        [ent("bank_transaction", -150000)],
        [ent("gl_entry", 150000)],
    )
    assert not auto, "a global sign flip changed the verdict; the rule is not sign-symmetric"
    assert len(review) == 1 and review[0].sign_conflict is True


def test_sign_conflict_reaches_review_even_with_no_date_or_counterparty_signal():
    """THE BOUNDARY, and the reason the downgrade is a flag rather than just
    `and sign_ok` on the auto_link branch.

    Strip the date and counterparty signal and an is_exact group scores
    0.5*1.0 + 0.2*0 + 0.3*0 = 0.500 against a review_floor of 0.500 — decided by
    a float comparison. A bare conjunct on the first branch would let this class
    fall out of BOTH queues, which is worse than the bug being fixed: an
    unmatched entity is reported as 'nothing to reconcile here', while an
    over-confident link at least appears in a queue."""
    auto, review, unmatched = route_group(
        [ent("invoice_line_item", 150000, txn_date=None, counterparty=None)],
        [ent("bank_transaction", 150000, txn_date=None, counterparty=None)],
        [ent("gl_entry", -150000, txn_date=None, counterparty=None)],
    )
    assert not auto, (
        "auto_linked a sign-conflicting group with no date or counterparty "
        "signal: %s" % [(g.link_confidence, g.status) for g in auto])
    assert len(review) == 1, (
        "a sign-conflicting group scored exactly at review_floor fell out of both "
        "queues (unmatched=%d)" % len(unmatched))
    assert review[0].sign_conflict is True


# ---- 2. the deliberate non-assertions (GUARDS: pass on both trees) ----

def test_invoice_bank_opposite_still_auto_links_gl_with_invoice():
    """GUARD. Convention A (invoice +, bank −, GL +) — what _score_group's and
    decimal_math's comments claim. Must be untouched."""
    auto, _, _ = route_group(
        [ent("invoice_line_item", 89900)],
        [ent("bank_transaction", -89900)],
        [ent("gl_entry", 89900)],
    )
    assert len(auto) == 1, "convention A group was not auto_linked"
    assert auto[0].sign_conflict is False
    assert auto[0].link_confidence == 1.0


def test_invoice_bank_opposite_still_auto_links_gl_with_bank():
    """GUARD. Convention C (invoice +, bank −, GL −) — a by-the-book bank
    reconciliation where the GL leg is the CASH account line rather than the
    expense line. Which one a firm uses is a fact about its chart of accounts,
    unknowable from this module, so the gate must assert NOTHING here. This is
    the test that would go red if someone widened the rule to 'GL must agree
    with the invoice'."""
    auto, _, _ = route_group(
        [ent("invoice_line_item", 89900)],
        [ent("bank_transaction", -89900)],
        [ent("gl_entry", -89900)],
    )
    assert len(auto) == 1, (
        "downgraded a convention-C group; the gate has been widened past what "
        "the codebase can justify")
    assert auto[0].sign_conflict is False


def test_all_three_same_sign_still_auto_links():
    """GUARD. Convention B — what the real fixtures actually produce. The
    fixtures' own test covers this end-to-end; this pins it at the unit."""
    for sign in (1, -1):
        auto, _, _ = route_group(
            [ent("invoice_line_item", sign * 47550)],
            [ent("bank_transaction", sign * 47550)],
            [ent("gl_entry", sign * 47550)],
        )
        assert len(auto) == 1, "sign=%d: aligned group was not auto_linked" % sign
        assert auto[0].sign_conflict is False


def test_two_leg_bank_gl_group_gets_no_sign_constraint():
    """GUARD. bank↔GL is OPPOSITE under convention A and SAME under B and C, so
    a 2-member group (deposits, bank fees — no invoice leg) carries no decidable
    constraint at all. Both directions must auto_link."""
    for gl_sign in (1, -1):
        auto, _, _ = route_group(
            [],
            [ent("bank_transaction", -25000)],
            [ent("gl_entry", gl_sign * 25000)],
        )
        assert len(auto) == 1, (
            "gl_sign=%+d: a 2-leg bank+GL group was downgraded on sign, which "
            "no convention in this repo licenses" % gl_sign)
        assert auto[0].sign_conflict is False


def test_zero_netting_invoice_leg_is_not_a_sign_conflict():
    """GUARD, and rule 10. An invoice plus its full credit note nets to zero. The
    leg is PRESENT by membership (verify_worker.go decides presence with
    BOOL_OR(m.role=...), not by total != 0), and a total of zero has no sign to
    compare. Downgrading it would break the credit-note case in the name of
    sign hygiene."""
    from graph.link import _sign_pattern_ok

    ok, reason = _sign_pattern_ok(0, 150000, -150000, True, True, True)
    assert ok, "zero-netting invoice leg reported a sign conflict: %s" % reason
    ok, reason = _sign_pattern_ok(150000, 0, -150000, True, True, True)
    assert ok, "zero-netting bank leg reported a sign conflict: %s" % reason
    ok, reason = _sign_pattern_ok(150000, 150000, 0, True, True, True)
    assert ok, "zero-netting GL leg reported a sign conflict: %s" % reason


# ---- 3. the shared contract with the Rust tier (GUARDS) ----

def test_amounts_match_stays_magnitude_only():
    """GUARD, and the point of it is the failure it prevents in the OTHER tier.
    If _amounts_match became sign-aware, Python would stop proposing candidates
    that Rust would reconcile — the 139/600 tier disagreement in reverse, and
    invisible, because a suppressed candidate leaves no row anywhere. The sign
    question belongs in _sign_pattern_ok at routing time, where it can only
    downgrade something that is already a group."""
    assert _amounts_match(150000, -150000, 0), (
        "_amounts_match rejected equal magnitudes with opposite signs; it is "
        "magnitude-only by contract with compute_three_way_variance")
    assert _amounts_match(-150000, 150000, 0)
    assert _amounts_match(150000, 150000, 0)
    assert not _amounts_match(150000, -150001, 0)
    assert _amounts_match(150000, -150001, 1)


def test_rust_verifier_still_compares_absolute_values():
    """GUARD, source-text. _amounts_match is magnitude-only BECAUSE
    compute_three_way_variance is. If someone makes the Rust side sign-aware,
    the reason for the Python contract evaporates and this file's whole premise
    goes with it — so that change must land here as a red test rather than as a
    silent divergence between the two tiers.

    This asserts source text, not behaviour: cargo is not available in this
    environment. Same compromise, and the same disclosure, as
    verify_worker_test.go's source-invariant tests."""
    path = os.path.join(REPO, "services", "verification", "src", "decimal_math", "mod.rs")
    with open(path, encoding="utf-8") as fh:
        src = fh.read()
    start = src.index("fn compute_three_way_variance")
    body = src[start:src.index("\n}", start)]
    pushes = re.findall(r"variances\.push\((.*?)\);", body)
    assert len(pushes) == 3, (
        "expected 3 pairwise comparisons in compute_three_way_variance, found "
        "%d: %r" % (len(pushes), pushes))
    for expr in pushes:
        assert expr.count(".abs()") == 3, (
            "compute_three_way_variance no longer takes absolute values on both "
            "operands and the difference: %r. If that is intended, _amounts_match "
            "and _sign_pattern_ok in graph/link.py have to change with it." % expr)


def test_fixtures_are_still_sign_aligned():
    """GUARD, and it exists because a comment that records a measurement can rot
    while every test stays green. The narrowness of _sign_pattern_ok is justified
    by ONE observation: the real fixtures parse to legs that are sign-for-sign
    identical, so 'invoice↔bank must be opposite' is false in this codebase. If
    the fixtures or their parsing are ever re-signed, that justification is gone
    and this goes red instead of the comment quietly becoming wrong.

    Reuses test_pilot_fixtures.entities_from_fixtures on purpose: the claim being
    guarded is about the legs as the linker receives them, and that helper is
    what produces them (sample_gl.csv is a QuickBooks Debit/Credit pair, not a
    signed column, so reading the sign off the file text would be measuring
    something else)."""
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from test_pilot_fixtures import entities_from_fixtures, FIXTURES_DIR

    invoices, banks, gls = entities_from_fixtures(
        os.path.join(FIXTURES_DIR, "sample_invoice.csv"),
        os.path.join(FIXTURES_DIR, "sample_bank.ofx"),
        os.path.join(FIXTURES_DIR, "sample_gl.csv"),
    )
    inv_amts = [e.amount_cents for e in invoices]
    bank_amts = [e.amount_cents for e in banks]
    gl_amts = [e.amount_cents for e in gls]
    assert inv_amts, "sample_invoice.csv parsed to no entities"

    def signs(xs):
        return [(1 if v > 0 else -1 if v < 0 else 0) for v in xs]

    assert signs(inv_amts) == signs(bank_amts) == signs(gl_amts), (
        "the fixture legs are no longer sign-aligned, so _sign_pattern_ok's "
        "narrowness is no longer justified by measurement.\n"
        "  invoice %r\n  bank    %r\n  gl      %r" % (inv_amts, bank_amts, gl_amts))
    assert -1 in signs(inv_amts), (
        "the fixtures no longer contain a negative row, so they no longer "
        "demonstrate that sign encodes refund-direction rather than side")
