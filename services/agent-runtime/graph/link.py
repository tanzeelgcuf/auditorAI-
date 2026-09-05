# services/agent-runtime/graph/link.py
# Cross-linking algorithm — fuzzy matches entities across document types.
# This is retrieval/scoring, NOT financial calculation.
# Follows docs 06 §2 + 09 §1: bounded combinatorial group matching.

import structlog
import jellyfish
from typing import Optional, List, Tuple
from itertools import combinations
from datetime import date

from .schema import ExtractedEntity, BookConfig, ReconciliationGroup, GraphState

logger = structlog.get_logger()

MAX_GROUP_SIZE = 5
DATE_WINDOW_DAYS = 3
# Invoices are dated when issued; payment lands later (net terms). So an
# invoice may legitimately PRECEDE its payment by days-to-weeks, but can never
# FOLLOW it. INVOICE_LOOKBACK_DAYS widens the candidate window for invoices
# before the payment; _dates_match keeps enforcing the tight 3-day window for
# same-side legs (invoice↔invoice = the affected rows stay real). The rejection
# for invoice-after-payment is an asymmetric cap that catches a real matching
# error (doc 11 Round 5) without admitting spurious dates.
INVOICE_LOOKBACK_DAYS = 14
# Jaro-Winkler floor for two names to be "the same vendor". 0.80 admitted
# confusable pairs (Stress Set #2: "Sunrise Landscaping & Grounds" vs "Sunrise
# Landscape Maintenance" scored 0.87 and cross-linked). Correct pairs score
# >=0.95; genuinely different vendors score <=0.87. 0.90 cleanly separates them
# while Riverside/QBO exact-name pairs (>=0.99) are unaffected.
COUNTERPARTY_THRESHOLD = 0.90
# Pass-5 mismatch detection: bank↔GL same date/cp but amounts disagree. Only flag
# it when the disagreement is a plausible POSTING ERROR (small relative gap), not
# two unrelated transactions with the same counterparty that happen to share a
# date. 50% of the larger amount is the ceiling — a receivable posted for less
# than half its value is a different transaction, not a typo.
MISMATCH_RATIO = 0.50


def _entity_key(e: ExtractedEntity) -> tuple:
    return (str(e.id), e.entity_type)


def _amounts_match(a: int, b: int, tolerance: int) -> bool:
    """Check if two amounts match within tolerance (cents), on MAGNITUDE ONLY.

    Compares absolute values: bank transactions are debits (negative) while GL
    entries are credits (positive) for the same underlying transaction — the
    sign is a side convention, not a different amount.

    DO NOT make this sign-aware. The contract is shared with the deterministic
    tier: `compute_three_way_variance`
    (`services/verification/src/decimal_math/mod.rs`, comment at :81-88, the
    three `.abs()` comparisons at :89-97) takes absolute values on all three
    pairs, and its comment names this function as the reason. Rule 9 says that
    tier disposes. If this predicate started rejecting a sign pattern the
    verifier accepts, Python would silently withhold candidates Rust would have
    reconciled — the same tier disagreement as the 139/600 bug, in the opposite
    direction and harder to see, because a suppressed candidate leaves no row.

    The sign question is real, and it is asked in `_sign_pattern_ok`, at
    ROUTING time, where it can only downgrade.
    """
    return abs(abs(a) - abs(b)) <= tolerance


# THE SIGN CONVENTION IN THIS REPO IS NOT ONE CONVENTION. IT IS TWO, AND BOTH
# ARE EXERCISED BY PASSING TESTS. Established 2026-09-06 by measurement, after
# first writing the gate the prose implies and watching it break the fixtures.
#
# What the PROSE said — two places, agreeing with each other, and both wrong to
# state it as settled:
#   _score_group, this file             "a billed invoice +, its bank debit −,
#                                        its GL credit +"  (CORRECTED in place
#                                        2026-09-06; see the note there)
#   compute_three_way_variance,         "bank payment and GL entry carry the
#   verification/decimal_math (:82-84)   opposite sign by convention
#                                        (bank debit -, GL credit +)"
#
# What the REAL FIXTURES do — tests/fixtures/sample_invoice.csv +
# sample_bank.ofx + sample_gl.csv, the only data here that comes from real file
# formats rather than a hand-written literal. Parsed through
# entities_from_fixtures, the three legs are sign-for-sign IDENTICAL:
#   invoice [150000, 25000, 8999, -50000, 320000, 120000, 47550, 199999, 64275, 87500]
#   bank    [150000, 25000, 8999, -50000, 320000, 120000, 47550, 199999, 64275, 87500]
#   gl      [150000, 25000, 8999, -50000, 320000, 120000, 47550, 199999, 64275, 87500]
# The fixture builder says why, and says it deliberately: "OFX expresses the
# refund as a positive CREDIT while the invoice/GL express it as a negative
# refund. Align to the invoice sign convention." So sign encodes DIRECTION
# (charge vs refund), not SIDE. tests/test_link.py is all-positive throughout,
# the same way; tests/test_link_tolerance.py hand-builds the prose convention.
#
# MEASURED CONSEQUENCE. A gate enforcing the prose convention (invoice↔bank
# opposite) fails 10 of 77 tests, including
# test_sample_invoice_bank_ofx_produces_valid_reconciliation_group — the real
# one. That is not a gate finding bugs; that is a gate encoding a guess.
#
# So the three readings that are live, none of them excludable from here:
#   A "side-encoded"  inv +, bank −, gl +   → inv↔bank OPP,  inv↔gl SAME, bank↔gl OPP
#   B "direction-only" inv +, bank +, gl +  → inv↔bank SAME, inv↔gl SAME, bank↔gl SAME
#   C "bank-rec"      inv +, bank −, gl −   → inv↔bank OPP,  inv↔gl OPP,  bank↔gl SAME
# B is what the fixtures do. A is what the comments claim. C is what a
# by-the-book bank reconciliation does when the `gl` leg is the CASH account
# line rather than the expense/revenue line, which is a fact about the
# customer's chart of accounts and is not knowable from this file.
#
# WHAT IS STILL DECIDABLE WITHOUT PICKING ONE. Enumerate all sign patterns for
# three non-zero legs (up to a global flip, which preserves every pairwise
# relation): (+,+,+) is B, (+,−,+) is A, (+,−,−) is C, and (+,+,−) is none of
# them. Exactly one pattern is impossible under all three, and it states
# compactly:
#
#     IF the invoice and bank legs AGREE in sign, the GL leg must agree too.
#
# When invoice and bank DISAGREE, A permits gl-with-invoice and C permits
# gl-with-bank, so there is no constraint and this asserts none. Two-leg groups
# (bank+GL, no invoice) get no constraint either — bank↔gl is OPP under A and
# SAME under B and C. Narrow, and true regardless of which convention a book
# uses, which is the trade being made deliberately.


def _sign_pattern_ok(inv_total: int, bank_total: int, gl_total: int,
                     has_inv: bool, has_bank: bool, has_gl: bool) -> Tuple[bool, str]:
    """Is this group's sign pattern possible under ANY of conventions A/B/C above?

    Returns (ok, reason). Enforces exactly one rule, the only one that survives
    not knowing which convention the book uses: **if the invoice and bank legs
    agree in sign, the GL leg must agree with them.**

    THE DEFECT THIS CLOSES. `_amounts_match` compares magnitudes, so a leg of the
    right SIZE on the wrong SIDE is variance 0 — and a 1:1:1 group of those
    scores `is_exact` True, confidence 1.0, and auto-links with nobody looking.
    Concretely, at the fixtures' own convention: invoice +150000, bank +150000,
    GL -150000 reconciled clean. The likely readings are a double-entry posted to
    the wrong side, or a refund recorded against the charge it reverses. Both are
    findings; both were indistinguishable from a clean match.

    WHAT IT DELIBERATELY DOES NOT ASSERT. Nothing when invoice and bank disagree
    (A and C both live, pointing opposite ways). Nothing about 2-leg bank+GL
    groups. Nothing when a leg totals ZERO — that leg has no sign to compare, and
    it is rule 10's case: an invoice plus its full credit note nets to zero, is
    PRESENT by membership, and must not be downgraded for lacking a sign.
    Widening any of these needs the book's convention, which means a per-book
    config field and a migration, not a guess in a predicate.
    """
    if not (has_inv and has_bank and has_gl):
        return True, ""
    if inv_total == 0 or bank_total == 0 or gl_total == 0:
        return True, ""  # no sign to compare — rule 10, not a failure
    inv_pos = inv_total > 0
    bank_pos = bank_total > 0
    gl_pos = gl_total > 0
    if inv_pos != bank_pos:
        return True, ""  # conventions A and C disagree here; assert nothing
    if gl_pos != inv_pos:
        side = "positive" if inv_pos else "negative"
        return False, (
            "invoice and bank are both %s but GL is %s; no sign convention in "
            "this codebase permits the GL leg alone on the other side"
            % (side, "negative" if inv_pos else "positive"))
    return True, ""


def _dates_match(a: Optional[date], b: Optional[date]) -> bool:
    """Check if two dates are within the matching window.

    a is the invoice date, b the payment date. Same-side legs (invoice↔invoice,
    bank↔bank, GL↔GL) use the tight symmetric window; an invoice may precede its
    payment (net terms) by up to INVOICE_LOOKBACK_DAYS but never follow it.
    """
    if a is None or b is None:
        return True  # no date = don't filter out
    delta = (b - a).days
    if delta < 0:  # payment before invoice — impossible
        return False
    if delta <= DATE_WINDOW_DAYS:  # tight window, exact-ish
        return True
    # invoice precedes payment by more than the tight window — allow net terms
    return delta <= INVOICE_LOOKBACK_DAYS and a <= b


def _counterparties_match(a: Optional[str], b: Optional[str]) -> bool:
    """Check if counterparty names could match using Jaro-Winkler."""
    if a is None or b is None:
        return True
    return jellyfish.jaro_winkler_similarity(a.lower(), b.lower()) >= COUNTERPARTY_THRESHOLD


def _compute_date_score(dates: List[date]) -> float:
    """Compute date proximity score within the window (1.0 at exact match → 0 at window edge)."""
    if len(dates) < 2:
        return 0.0
    sorted_dates = sorted(dates)
    max_gap = (sorted_dates[-1] - sorted_dates[0]).days
    if max_gap == 0:
        return 1.0
    return max(0.0, 1.0 - (max_gap / DATE_WINDOW_DAYS))


def _compute_counterparty_score(names: List[Optional[str]]) -> float:
    """Compute average Jaro-Winkler similarity across counterparty pairs."""
    present = [n for n in names if n]
    if len(present) < 2:
        return 0.0
    scores = []
    for i in range(len(present)):
        for j in range(i + 1, len(present)):
            scores.append(jellyfish.jaro_winkler_similarity(
                present[i].lower(), present[j].lower()
            ))
    return sum(scores) / len(scores) if scores else 0.0


def _score_group(
    invoice_entities: List[ExtractedEntity],
    bank_entities: List[ExtractedEntity],
    gl_entities: List[ExtractedEntity],
    config: BookConfig,
    is_exact: bool,
) -> float:
    """Compute link confidence score 0.0-1.0.

    Weights (doc 06 §2): amount 0.5, date 0.2, counterparty 0.3.
    """
    if is_exact:
        return 1.0

    total_inv = sum(e.amount_cents for e in invoice_entities)
    total_bank = sum(e.amount_cents for e in bank_entities)
    total_gl = sum(e.amount_cents for e in gl_entities)

    # Amount match score (0.5). Compare only PRESENT legs — a 2-member group
    # (bank+GL, no invoice) must not be penalized by the absent invoice leg.
    #
    # Two corrections here, the same pair found in score_and_route's is_exact:
    #
    # 1. Variance is |‖a‖ − ‖b‖|, not |a − b|. This used signed subtraction.
    #    |89900 − (−89899)| is 179799, so a group one cent out of tolerance
    #    scored avg_variance ≈ 1.33 × max_amt, amount_score clamped to 0.0, and
    #    the whole non-exact path collapsed to 0.2·date + 0.3·counterparty ≤ 0.5.
    #    At review_floor 0.50 that sits exactly on the boundary: a slightly fuzzy
    #    counterparty (0.95 → 0.285) drops it to 0.485 and score_and_route then
    #    routes it to NEITHER queue — a real over-tolerance discrepancy
    #    disappearing rather than being reviewed. _amounts_match and
    #    services/verification's compute_three_way_variance both compare absolute
    #    values; this now does too, so the near-miss score degrades smoothly with
    #    the actual gap.
    #
    #    CORRECTED 2026-09-06. This comment used to justify the abs() by asserting
    #    "every legitimate 3-way group carries opposite signs by convention (a
    #    billed invoice +, its bank debit −, its GL credit +)". That is not
    #    established: the repo's own fixtures (services/ingestion/test_fixtures,
    #    parsed by tests/test_pilot_fixtures.entities_from_fixtures) produce
    #    invoice, bank and GL legs that are sign-for-sign IDENTICAL, and their
    #    builder aligns them deliberately. See the block above _sign_pattern_ok:
    #    this codebase holds TWO sign conventions and both have passing tests.
    #    The abs() is right either way — that is the actual argument for it, and
    #    it is a stronger one than the convention claim it replaced.
    #
    # 2. Presence is membership, not a non-zero total. A leg whose amounts net to
    #    zero (an invoice and its full credit note) is present and must be
    #    compared, because verify_worker.go's BOOL_OR(m.role=…) tells the
    #    verification tier it is present and it will be compared there.
    legs = [
        ("inv", total_inv, bool(invoice_entities)),
        ("bank", total_bank, bool(bank_entities)),
        ("gl", total_gl, bool(gl_entities)),
    ]
    present = [(label, value) for label, value, is_present in legs if is_present]
    amounts = [abs(v) for _, v in present]
    max_amt = max(amounts) if amounts else 1
    variances = []
    for i in range(len(present)):
        for j in range(i + 1, len(present)):
            variances.append(abs(abs(present[i][1]) - abs(present[j][1])))
    avg_variance = sum(variances) / len(variances) if variances else 0.0
    amount_score = max(0.0, 1.0 - (avg_variance / max_amt)) if max_amt > 0 else 0.0

    # Date proximity (0.2)
    dates = [
        e.transaction_date
        for e in invoice_entities + bank_entities + gl_entities
        if e.transaction_date
    ]
    date_score = _compute_date_score(dates)

    # Counterparty similarity (0.3)
    counterparties = [
        e.counterparty
        for e in invoice_entities + bank_entities + gl_entities
    ]
    cp_score = _compute_counterparty_score(counterparties)

    return 0.5 * amount_score + 0.2 * date_score + 0.3 * cp_score


def build_candidate_groups(
    invoices: List[ExtractedEntity],
    banks: List[ExtractedEntity],
    gls: List[ExtractedEntity],
    config: BookConfig,
) -> List[ReconciliationGroup]:
    """Build candidate reconciliation groups using bounded combinatorial search.

    doc 09 §1: groups, not 1:1:1 links. Handles:
    - one bank payment covering N invoices (bounded by MAX_GROUP_SIZE)
    - one invoice paid in N installments
    - ambiguous ties (multiple equally-plausible groupings all surface)
    """
    candidates: List[ReconciliationGroup] = []

    # Exclude voided entities from reconciliation entirely (doc 08 §5)
    invoices = [e for e in invoices if e.entity_subtype != "void"]
    banks = [e for e in banks if e.entity_subtype != "void"]
    gls = [e for e in gls if e.entity_subtype != "void"]

    # Pass 1: 1:1:1 exact fast path — filtered by date window + counterparty
    # (doc 06 §2: candidate search constrains amount AND date AND counterparty)
    for bank in banks:
        for inv in invoices:
            if not _dates_match(inv.transaction_date, bank.transaction_date):
                continue
            if not _counterparties_match(inv.counterparty, bank.counterparty):
                continue
            if not _amounts_match(inv.amount_cents, bank.amount_cents, config.tolerance_cents):
                continue
            for gl in gls:
                if not _dates_match(inv.transaction_date, gl.transaction_date):
                    continue
                if not _counterparties_match(inv.counterparty, gl.counterparty):
                    continue
                if not _amounts_match(inv.amount_cents, gl.amount_cents, config.tolerance_cents):
                    continue
                candidates.append(ReconciliationGroup(
                    client_book_id=config.id,
                    invoice_entity_ids=[inv.id],
                    bank_entity_ids=[bank.id],
                    gl_entity_ids=[gl.id],
                    link_confidence=1.0,
                    status="needs_review",
                ))

    # Pass 2: many-to-one — one bank, N invoices summing to bank amount
    for bank in banks:
        relevant = [
            inv for inv in invoices
            if _dates_match(inv.transaction_date, bank.transaction_date)
            and _counterparties_match(inv.counterparty, bank.counterparty)
        ]
        for size in range(2, min(MAX_GROUP_SIZE, len(relevant)) + 1):
            for combo in combinations(relevant, size):
                total = sum(e.amount_cents for e in combo)
                if _amounts_match(total, bank.amount_cents, config.tolerance_cents):
                    for gl in gls:
                        if _amounts_match(total, gl.amount_cents, config.tolerance_cents):
                            candidates.append(ReconciliationGroup(
                                client_book_id=config.id,
                                invoice_entity_ids=[e.id for e in combo],
                                bank_entity_ids=[bank.id],
                                gl_entity_ids=[gl.id],
                                link_confidence=0.0,
                                status="needs_review",
                            ))

    # Pass 3: one-to-many — one invoice, N banks summing to invoice amount.
    # The date window applies between group members (banks against each other),
    # not against the invoice date — installment payments legitimately span months.
    for inv in invoices:
        same_cp_banks = [
            bank for bank in banks
            if _counterparties_match(inv.counterparty, bank.counterparty)
        ]
        for size in range(2, min(MAX_GROUP_SIZE, len(same_cp_banks)) + 1):
            for combo in combinations(same_cp_banks, size):
                if not _combo_dates_within_window(combo):
                    continue
                total = sum(e.amount_cents for e in combo)
                if _amounts_match(total, inv.amount_cents, config.tolerance_cents):
                    for gl in gls:
                        if _amounts_match(total, gl.amount_cents, config.tolerance_cents):
                            candidates.append(ReconciliationGroup(
                                client_book_id=config.id,
                                invoice_entity_ids=[inv.id],
                                bank_entity_ids=[e.id for e in combo],
                                gl_entity_ids=[gl.id],
                                link_confidence=0.0,
                                status="needs_review",
                            ))

    # Pass 4: bank+GL two-member groups — transactions with no invoice leg
    # (deposits, bank fees) still reconcile bank against GL (doc 09 §1: a
    # group need not have all three legs).
    for bank in banks:
        for gl in gls:
            if not _dates_match(bank.transaction_date, gl.transaction_date):
                continue
            if not _counterparties_match(bank.counterparty, gl.counterparty):
                continue
            if not _amounts_match(bank.amount_cents, gl.amount_cents, config.tolerance_cents):
                continue
            candidates.append(ReconciliationGroup(
                client_book_id=config.id,
                invoice_entity_ids=[],
                bank_entity_ids=[bank.id],
                gl_entity_ids=[gl.id],
                link_confidence=0.0,
                status="needs_review",
            ))

    # Pass 5: same-date+counterparty bank↔GL pair whose AMOUNTS DO NOT MATCH.
    # The bank leg and GL leg describe the same real transaction (same date,
    # same counterparty) but differ in amount — e.g. a posting error left a
    # receivable recorded at 89400 when the bank cleared 89900. This is a
    # discrepancy a human must see, NOT a silent "unmatched". Mark it mismatch=True
    # so score_and_route routes it to needs_review regardless of the (hair-thin)
    # confidence threshold, and the verification tier computes the variance and
    # flags severity (doc 12 §2 / Round 5). Pass 4 already covered matching
    # pairs; this pass catches the mismatch case it would have dropped.
    for bank in banks:
        for gl in gls:
            if _amounts_match(bank.amount_cents, gl.amount_cents, config.tolerance_cents):
                continue  # already covered by pass 4
            if not _dates_match(bank.transaction_date, gl.transaction_date):
                continue
            if not _counterparties_match(bank.counterparty, gl.counterparty):
                continue
            # Bound the mismatch to plausible posting errors: the two amounts must
            # be within MISMATCH_RATIO of each other (89900 vs 89400 ✓, 99999 vs
            # 11111 ✗ — different transactions). Prevents flagging unrelated
            # same-cp/same-date pairs as discrepancies.
            larger = max(abs(bank.amount_cents), abs(gl.amount_cents))
            smaller = min(abs(bank.amount_cents), abs(gl.amount_cents))
            if smaller == 0 or (larger - smaller) / larger > MISMATCH_RATIO:
                continue
            # Attach any invoice legs that match the bank side (same cp, date,
            # and amount) — e.g. BCH-2291 $899.00 sits against bank -89900 while
            # GL mis-posted 89400. The invoice belongs in the group so the
            # verification tier sees the full 3-way variance, not a dangling
            # 2-member pair with an orphaned invoice (doc 12 §2).
            attached_invs = [
                inv.id for inv in invoices
                if _counterparties_match(inv.counterparty, bank.counterparty)
                and _dates_match(inv.transaction_date, bank.transaction_date)
                and _amounts_match(inv.amount_cents, bank.amount_cents, config.tolerance_cents)
            ]
            candidates.append(ReconciliationGroup(
                client_book_id=config.id,
                invoice_entity_ids=attached_invs,
                bank_entity_ids=[bank.id],
                gl_entity_ids=[gl.id],
                link_confidence=0.0,
                status="needs_review",
                mismatch=True,
            ))

    return _deduplicate_candidates(candidates)


def score_and_route(
    candidates: List[ReconciliationGroup],
    entities_by_id: dict,
    config: BookConfig,
) -> Tuple[List[ReconciliationGroup], List[ReconciliationGroup], List[ExtractedEntity]]:
    """Score candidate groups and route to auto_linked / needs_review / unmatched.

    Returns (auto_linked, needs_review, unmatched_entities).
    """
    # Normalize the entity map keys to strings (UUID objects and str both possible)
    normalized = {}
    for k, v in entities_by_id.items():
        normalized[str(k)] = v

    def lookup(eid) -> Optional[ExtractedEntity]:
        return normalized.get(str(eid))

    auto_linked: List[ReconciliationGroup] = []
    needs_review: List[ReconciliationGroup] = []
    matched_ids = set()

    for group in candidates:
        invs = [lookup(eid) for eid in group.invoice_entity_ids]
        banks = [lookup(eid) for eid in group.bank_entity_ids]
        gls = [lookup(eid) for eid in group.gl_entity_ids]
        invs = [e for e in invs if e is not None]
        banks = [e for e in banks if e is not None]
        gls = [e for e in gls if e is not None]

        # Skip groups with no present legs at all, but ALLOW 2-member groups
        # (bank+GL, no invoice leg — deposits, bank fees).
        if not banks or not gls:
            continue

        inv_total = sum(e.amount_cents for e in invs)
        bank_total = sum(e.amount_cents for e in banks)
        gl_total = sum(e.amount_cents for e in gls)

        # Exact = EVERY PAIR of present legs matches within tolerance (abs — sign
        # is a convention). A 2-member group (bank+GL, no invoice) is exact when
        # the two match; a 3-member group needs all three pairs.
        #
        # Two things here were wrong and both let a real variance through as
        # "exact", confidence 1.0:
        #
        # 1. This compared every leg against present_totals[0] only — a star, not
        #    all pairs. build_candidate_groups likewise gates invoice↔bank and
        #    invoice↔GL but never bank↔GL. Two legs each one tolerance off the
        #    invoice, in opposite directions, are 2× tolerance apart from each
        #    other and passed both checks. services/verification computes all
        #    three pairwise variances and takes the max, so the tiers disagreed:
        #    invoice +89900 / bank -89899 / GL +89901 at the default 1¢ tolerance
        #    was 1.0 "exact" here and a 2¢ exceeds_tolerance finding there.
        #    Measured over 600 randomised books: 139 auto_linked groups that the
        #    verification tier judged over tolerance.
        #
        # 2. Presence was `t != 0`, so a leg WITH members whose amounts net to
        #    zero (an invoice and its full credit note) counted as absent and was
        #    dropped from the comparison. The verification tier decides presence
        #    from membership (verify_worker.go BOOL_OR(m.role=...) → has_invoice),
        #    so it compares that leg as 0 against the full bank amount. Presence
        #    is now membership here too, which is the same question the DB asks.
        #
        # This is a candidate-quality gate, not the authority on disposition:
        # verify_worker.go downgrades an over-tolerance group regardless of what
        # this scores. Both exist because a group that is wrong here is also
        # wrong in the confidence number the audit trail records.
        present_totals = [
            t for t, present in (
                (inv_total, bool(invs)),
                (bank_total, bool(banks)),
                (gl_total, bool(gls)),
            ) if present
        ]
        is_exact = len(present_totals) >= 2 and all(
            _amounts_match(a, b, config.tolerance_cents)
            for a, b in combinations(present_totals, 2)
        )

        score = _score_group(invs, banks, gls, config, is_exact)
        group.link_confidence = round(score, 4)

        # SIGN GATE — downgrade-only, and the only place in this file that looks
        # at sign at all. `is_exact` above is magnitude-only by contract (see
        # _amounts_match), which means a leg of the right SIZE and the wrong SIDE
        # is variance 0, is_exact True, confidence 1.0 — auto-linked, no human.
        # At the fixtures' own convention: invoice +150000, bank +150000,
        # GL -150000 reconciled clean. That is a wrong-side posting or a refund
        # matched to the charge it reverses; both are findings, and both arrived
        # as clean reconciliations.
        #
        # It downgrades and never suppresses, for two independent reasons: rule 9
        # (the verification tier disposes; this tier may only lower a claim, never
        # raise or hide one), and because the repo demonstrably holds TWO sign
        # conventions — see the block above _sign_pattern_ok for the measurement.
        # The predicate is therefore deliberately narrow, and even where it fires
        # the cost of being wrong is review work, never a missed match.
        sign_ok, sign_reason = _sign_pattern_ok(
            inv_total, bank_total, gl_total, bool(invs), bool(banks), bool(gls))

        # WHY THE FLAG AND NOT JUST `and sign_ok` ON THE FIRST BRANCH. Adding the
        # conjunct alone would let a sign-contradicting group fall out of BOTH
        # queues on a boundary score, which is strictly worse than auto-linking
        # it: an unmatched entity is reported as "nothing to reconcile here",
        # while an over-confident link at least appears in a queue. is_exact
        # groups sit exactly on that boundary by construction — amount_score is
        # 1.0, so a group with no date and no counterparty signal scores
        # 0.5·1.0 = 0.500 against a review_floor of 0.500, decided by float
        # comparison. The `or group.sign_conflict` in the elif makes review the
        # floor for this class rather than something it can miss. Same shape as
        # the group.mismatch early-return above, and the same reason.
        #
        # `sign_conflict` is set only when the group WOULD have auto-linked
        # (is_exact), not on every sign oddity. A non-exact group already has a
        # variance to explain and is already headed for review or below the floor
        # on its own merits; flagging those too would widen a targeted downgrade
        # into a reclassification of candidates this change has no evidence about.
        group.sign_conflict = is_exact and not sign_ok
        if group.sign_conflict:
            # The reason reaches the LOG and nothing else. reconciliation_groups
            # has no column for it (infra/init.sql:220-241 — confidence, status,
            # group_scope, no downgrade reason) and the only INSERT path from this
            # tier is mcp.go create_entity_link, which carries status and scope.
            # So a reviewer sees a needs_review group with confidence 1.0 and no
            # stated cause. That is a real gap; it is a schema change plus a
            # writer change, not something to smuggle into this fix.
            logger.warning(
                "sign pattern contradicts convention — downgraded to needs_review",
                group_id=str(group.id),
                reason=sign_reason,
                inv_cents=inv_total, bank_cents=bank_total, gl_cents=gl_total,
                link_confidence=group.link_confidence,
            )

        # Pass-5 discrepancy candidates are mismatch BY CONSTRUCTION — the bank
        # and GL legs are the same real transaction whose amounts disagree. Route
        # them straight to needs_review regardless of confidence: a hair-thin
        # score drop (e.g. 0.4976 vs floor 0.50 from a trailing "." in a
        # counterparty) must not bury a real discrepancy as "unmatched".
        if group.mismatch:
            group.status = "needs_review"
            needs_review.append(group)
            for eid in group.invoice_entity_ids + group.bank_entity_ids + group.gl_entity_ids:
                matched_ids.add(str(eid))
            continue

        # CONFIDENCE IS NOT TOLERANCE, and `is_exact` is the tolerance question.
        #
        # This was `if score >= config.auto_link_threshold:` alone. The score
        # measures how likely these records describe the same transaction —
        # amount proximity, date proximity, counterparty similarity — and a 2¢
        # gap on an $899 invoice is 0.99998 of it. So a group whose legs are
        # KNOWN not to reconcile within the book's tolerance was auto-linked on
        # the strength of being obviously the same transaction, which it is. Those
        # are two different questions and only one of them was being asked.
        #
        # This is a deliberate change to documented routing (doc 06 §2 describes
        # the threshold, not this conjunct). The weights and thresholds are
        # untouched; what changes is that clearing the threshold is now necessary
        # and not sufficient. A non-exact group still gets its score, still ranks
        # in the review queue by it, and is still linked as a group — it just
        # requires a human to accept the variance. The alternative is a product
        # that reports a reconciliation as clean while its own verification tier
        # has an open over-tolerance finding against it.
        if score >= config.auto_link_threshold and is_exact and sign_ok:
            group.status = "auto_linked"
            auto_linked.append(group)
            for eid in group.invoice_entity_ids + group.bank_entity_ids + group.gl_entity_ids:
                matched_ids.add(str(eid))
        elif score >= config.review_floor or group.sign_conflict:
            group.status = "needs_review"
            needs_review.append(group)
            for eid in group.invoice_entity_ids + group.bank_entity_ids + group.gl_entity_ids:
                matched_ids.add(str(eid))

    # Unmatched = entities never appearing in any auto_linked/needs_review group
    unmatched = [
        e for e in normalized.values()
        if str(e.id) not in matched_ids
    ]

    return auto_linked, needs_review, unmatched


def _deduplicate_candidates(candidates: List[ReconciliationGroup]) -> List[ReconciliationGroup]:
    """Remove duplicate candidates.

    When a bank+GL pair is covered by BOTH a full 3-leg group (with invoice) and a
    2-member group (no invoice), prefer the fuller 3-leg group — it captures the
    many-to-many case. Key by the bank+GL entity set so 2-member groups don't
    duplicate a full group for the same legs.
    """
    by_legs: dict = {}
    for c in candidates:
        # Key = (bank ids, gl ids) — the legs that identify "these transactions reconcile".
        key = (tuple(sorted(str(x) for x in c.bank_entity_ids)),
               tuple(sorted(str(x) for x in c.gl_entity_ids)))
        existing = by_legs.get(key)
        if existing is None or len(c.invoice_entity_ids) > len(existing.invoice_entity_ids):
            by_legs[key] = c
    return list(by_legs.values())


def _combo_dates_within_window(entities: List[ExtractedEntity]) -> bool:
    """Check that all dated entities in a combo fall within the date window of each other."""
    dates = [e.transaction_date for e in entities if e.transaction_date]
    if len(dates) < 2:
        return True  # no dates = don't filter
    return (max(dates) - min(dates)).days <= DATE_WINDOW_DAYS


def _check_counterparty_alias(name_a: Optional[str], name_b: Optional[str]) -> bool:
    """Check if names match via exact or alias lookup.
    In production, this queries counterparty_aliases table.
    """
    if name_a and name_b and name_a.lower() == name_b.lower():
        return True
    return False


def cross_link(state: GraphState, config: BookConfig) -> GraphState:
    """LangGraph node: cross-link classified entities into reconciliation groups."""
    logger.info("cross-linking entities", batch_id=str(state.get("batch_id")))

    entities = state.get("classified_entities") or []
    if not entities:
        logger.warning("no classified entities to cross-link")
        return state

    entities_by_id = {str(e.id): e for e in entities}

    invoices = [e for e in entities if e.entity_type == "invoice_line_item"]
    banks = [e for e in entities if e.entity_type == "bank_transaction"]
    gls = [e for e in entities if e.entity_type == "gl_entry"]

    if not invoices or not banks or not gls:
        logger.warning("missing entity types for linking",
                       invoices=len(invoices), banks=len(banks), gls=len(gls))
        state["groups"] = []
        return state

    candidates = build_candidate_groups(invoices, banks, gls, config)
    auto_linked, needs_review, unmatched = score_and_route(candidates, entities_by_id, config)

    state["groups"] = auto_linked + needs_review
    state["unmatched"] = unmatched

    logger.info("cross-linking complete",
                auto_linked=len(auto_linked),
                needs_review=len(needs_review),
                unmatched=len(unmatched),
                total_candidates=len(candidates))

    return state
