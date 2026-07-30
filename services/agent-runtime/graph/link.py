# services/agent-runtime/graph/link.py
# Cross-linking algorithm — fuzzy matches entities across document types.
# This is retrieval/scoring, NOT financial calculation. No arithmetic on monetary values.
# Follows docs 06 §2 + 09 §1: bounded combinatorial group matching.

import structlog
import jellyfish
from typing import Optional, Iterator
from itertools import combinations
from dataclasses import dataclass

from .schema import ExtractedEntity, BookConfig, GraphState

logger = structlog.get_logger()

MAX_GROUP_SIZE = 5
DATE_WINDOW_DAYS = 3


@dataclass
class MatchCandidate:
    invoice_entities: list[ExtractedEntity]
    bank_entities: list[ExtractedEntity]
    gl_entities: list[ExtractedEntity]
    confidence: float
    is_exact: bool


async def cross_link(state: GraphState, config: BookConfig) -> GraphState:
    """Cross-link entities across document types.

    Algorithm (doc 06 §2 + doc 09 §1):
    1. Start from unmatched bank_transactions (or invoice_line_items)
    2. Search for bounded combinatorial multi-set matches
    3. Score by amount/date/counterparty proximity
    4. Route to needs_review or auto_linked based on thresholds
    """
    logger.info("cross-linking entities", batch_id=str(state.batch_id))

    if len(state.classified_entities) < 3:
        logger.warning("too few entities to cross-link",
                        count=len(state.classified_entities))
        return state

    # Separate entities by type
    invoices = [e for e in state.classified_entities if e.entity_type == "invoice_line_item"]
    banks = [e for e in state.classified_entities if e.entity_type == "bank_transaction"]
    gls = [e for e in state.classified_entities if e.entity_type == "gl_entry"]

    # Build candidate groups
    groups = _build_reconciliation_groups(invoices, banks, gls, config)

    # Score each group
    scored = []
    for group in groups:
        score = _score_group(group, config)
        if score > 0:
            scored.append((group, score))

    # Route based on thresholds
    auto_linked = []
    needs_review = []

    for group, score in scored:
        if score >= config.auto_link_threshold:
            auto_linked.append(group)
        elif score >= config.review_floor:
            needs_review.append(group)

    logger.info("cross-linking complete",
                 auto_linked=len(auto_linked),
                 needs_review=len(needs_review),
                 total_candidates=len(scored))

    return state


def _build_reconciliation_groups(
    invoices: list[ExtractedEntity],
    banks: list[ExtractedEntity],
    gls: list[ExtractedEntity],
    config: BookConfig,
) -> list[MatchCandidate]:
    """Build candidate reconciliation groups using bounded combinatorial search.

    For each unmatched bank_transaction, search for multi-sets of invoice_line_items
    whose amounts sum within tolerance (up to MAX_GROUP_SIZE items).
    """
    candidates: list[MatchCandidate] = []

    for bank in banks:
        # Find single invoice matches (fast path)
        for inv in invoices:
            if _amounts_match(inv.amount_cents, bank.amount_cents, config.tolerance_cents):
                for gl in gls:
                    if _amounts_match(inv.amount_cents, gl.amount_cents, config.tolerance_cents):
                        candidates.append(MatchCandidate(
                            invoice_entities=[inv],
                            bank_entities=[bank],
                            gl_entities=[gl],
                            confidence=0.0,  # computed later
                            is_exact=True,
                        ))

        # Multi-invoice matches (combinatorial, bounded)
        relevant_invs = [
            inv for inv in invoices
            if _dates_match(inv.transaction_date, bank.transaction_date)
            and _counterparties_match(inv.counterparty, bank.counterparty)
        ]

        for size in range(2, min(MAX_GROUP_SIZE, len(relevant_invs)) + 1):
            for combo in combinations(relevant_invs, size):
                total = sum(e.amount_cents for e in combo)
                if _amounts_match(total, bank.amount_cents, config.tolerance_cents):
                    # Find matching GL entries
                    for gl in gls:
                        if _amounts_match(total, gl.amount_cents, config.tolerance_cents):
                            candidates.append(MatchCandidate(
                                invoice_entities=list(combo),
                                bank_entities=[bank],
                                gl_entities=[gl],
                                confidence=0.0,
                                is_exact=False,
                            ))

    # Deduplicate by entity IDs
    return _deduplicate_candidates(candidates)


def _score_group(group: MatchCandidate, config: BookConfig) -> float:
    """Compute link confidence score: 0.0-1.0.

    Weights (doc 06 §2):
    - amount_match: 0.5
    - date_proximity: 0.2
    - counterparty_similarity: 0.3
    """
    if group.is_exact:
        return 1.0

    total_inv = sum(e.amount_cents for e in group.invoice_entities)
    total_bank = sum(e.amount_cents for e in group.bank_entities)
    total_gl = sum(e.amount_cents for e in group.gl_entities)

    # Amount match score (0.5 weight)
    amounts = [total_inv, total_bank, total_gl]
    max_amt = max(abs(a) for a in amounts) if any(amounts) else 1
    variances = [abs(total_inv - total_bank), abs(total_inv - total_gl), abs(total_bank - total_gl)]
    avg_variance = sum(variances) / len(variances)
    amount_score = max(0, 1.0 - (avg_variance / max_amt)) if max_amt > 0 else 0.0

    # Date proximity score (0.2 weight)
    dates = [e.transaction_date for e in group.invoice_entities + group.bank_entities + group.gl_entities if e.transaction_date]
    date_score = _compute_date_score(dates) if len(dates) >= 2 else 0.0

    # Counterparty similarity score (0.3 weight)
    counterparties = [e.counterparty for e in group.invoice_entities + group.bank_entities + group.gl_entities if e.counterparty]
    cp_score = _compute_counterparty_score(counterparties) if len(counterparties) >= 2 else 0.0

    return 0.5 * amount_score + 0.2 * date_score + 0.3 * cp_score


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
    return jellyfish.jaro_winkler_similarity(a.lower(), b.lower()) >= 0.8


def _compute_date_score(dates: list) -> float:
    """Compute date proximity score within the window."""
    if len(dates) < 2:
        return 0.0
    sorted_dates = sorted(dates)
    max_gap = (sorted_dates[-1] - sorted_dates[0]).days
    if max_gap == 0:
        return 1.0
    return max(0, 1.0 - (max_gap / DATE_WINDOW_DAYS))


def _compute_counterparty_score(names: list[str]) -> float:
    """Compute average Jaro-Winkler similarity across counterparty pairs."""
    if len(names) < 2:
        return 0.0
    scores = []
    for i in range(len(names)):
        for j in range(i + 1, len(names)):
            if names[i] and names[j]:
                scores.append(jellyfish.jaro_winkler_similarity(
                    names[i].lower(), names[j].lower()
                ))
    return sum(scores) / len(scores) if scores else 0.0


def _deduplicate_candidates(candidates: list[MatchCandidate]) -> list[MatchCandidate]:
    """Remove duplicate candidates (same entity ID sets)."""
    seen = set()
    unique = []
    for c in candidates:
        key = frozenset(
            [(e.id, e.entity_type) for e in c.invoice_entities + c.bank_entities + c.gl_entities]
        )
        if key not in seen:
            seen.add(key)
            unique.append(c)
    return unique


def _check_counterparty_alias(name_a: Optional[str], name_b: Optional[str]) -> bool:
    """Check if names match via exact or alias lookup.
    In production, this queries counterparty_aliases table.
    """
    if name_a and name_b and name_a.lower() == name_b.lower():
        return True
    return False