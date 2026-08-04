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
    """Check if two amounts match within tolerance (cents).

    Compares absolute values: bank transactions are debits (negative) while GL
    entries are credits (positive) for the same underlying transaction — the
    sign is a side convention, not a different amount.
    """
    return abs(abs(a) - abs(b)) <= tolerance


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
    present = [("inv", total_inv), ("bank", total_bank), ("gl", total_gl)]
    amounts = [abs(v) for _, v in present if v != 0]
    max_amt = max(amounts) if amounts else 1
    variances = []
    for i in range(len(present)):
        for j in range(i + 1, len(present)):
            li, vi = present[i]
            lj, vj = present[j]
            if vi != 0 and vj != 0:
                variances.append(abs(vi - vj))
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
        # Exact = every PRESENT leg matches within tolerance (abs — sign is a
        # convention). A 2-member group (bank+GL, no invoice) is exact when the
        # two match; a 3-member group needs all three.
        present_totals = [t for t in (inv_total, bank_total, gl_total) if t != 0]
        is_exact = len(present_totals) >= 2 and all(
            _amounts_match(present_totals[0], t, config.tolerance_cents)
            for t in present_totals[1:]
        )

        score = _score_group(invs, banks, gls, config, is_exact)
        group.link_confidence = round(score, 4)

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

        if score >= config.auto_link_threshold:
            group.status = "auto_linked"
            auto_linked.append(group)
            for eid in group.invoice_entity_ids + group.bank_entity_ids + group.gl_entity_ids:
                matched_ids.add(str(eid))
        elif score >= config.review_floor:
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
