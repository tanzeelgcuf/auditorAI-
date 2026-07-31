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
COUNTERPARTY_THRESHOLD = 0.80


def _entity_key(e: ExtractedEntity) -> tuple:
    return (str(e.id), e.entity_type)


def _amounts_match(a: int, b: int, tolerance: int) -> bool:
    """Check if two amounts match within tolerance (cents)."""
    return abs(a - b) <= tolerance


def _dates_match(a: Optional[date], b: Optional[date]) -> bool:
    """Check if two dates are within the matching window."""
    if a is None or b is None:
        return True  # no date = don't filter out
    return abs((a - b).days) <= DATE_WINDOW_DAYS


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

    # Amount match score (0.5)
    amounts = [total_inv, total_bank, total_gl]
    max_amt = max(abs(a) for a in amounts) if any(amounts) else 1
    variances = [
        abs(total_inv - total_bank),
        abs(total_inv - total_gl),
        abs(total_bank - total_gl),
    ]
    avg_variance = sum(variances) / len(variances)
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

        if not invs or not banks or not gls:
            continue

        inv_total = sum(e.amount_cents for e in invs)
        bank_total = sum(e.amount_cents for e in banks)
        gl_total = sum(e.amount_cents for e in gls)
        # Exact = all three grouped totals match within tolerance (any group size)
        is_exact = (
            _amounts_match(inv_total, bank_total, config.tolerance_cents)
            and _amounts_match(inv_total, gl_total, config.tolerance_cents)
        )

        score = _score_group(invs, banks, gls, config, is_exact)
        group.link_confidence = round(score, 4)

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
    """Remove duplicate candidates (same entity ID sets)."""
    seen = set()
    unique = []
    for c in candidates:
        key = frozenset(
            list(c.invoice_entity_ids) + list(c.bank_entity_ids) + list(c.gl_entity_ids)
        )
        if key not in seen:
            seen.add(key)
            unique.append(c)
    return unique


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
