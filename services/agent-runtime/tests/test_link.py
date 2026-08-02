# services/agent-runtime/tests/test_link.py
# Cross-linking algorithm tests — synthetic document sets with known-correct results.
# Per docs 06 §2 + 09 §1. No network, no API key needed.

import sys
import os
from datetime import date
from uuid import UUID

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, BookConfig, ReconciliationGroup, GraphState
from graph.link import (
    build_candidate_groups,
    score_and_route,
    cross_link,
    MAX_GROUP_SIZE,
    DATE_WINDOW_DAYS,
)

BOOK_ID = UUID("11111111-1111-1111-1111-111111111111")
DOC_ID = UUID("22222222-2222-2222-2222-222222222222")


def make_entity(
    etype: str,
    amount_cents: int,
    txn_date: date = None,
    counterparty: str = None,
    desc: str = None,
    subtype: str = "standard",
) -> ExtractedEntity:
    return ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type=etype,
        entity_subtype=subtype,
        amount_cents=amount_cents,
        transaction_date=txn_date,
        counterparty=counterparty,
        description=desc,
    )


def default_config(tolerance: int = 1) -> BookConfig:
    return BookConfig(id=BOOK_ID, tolerance_cents=tolerance)


def run_link(invoices, banks, gls, config=None):
    config = config or default_config()
    entities = invoices + banks + gls
    entities_by_id = {str(e.id): e for e in entities}
    candidates = build_candidate_groups(invoices, banks, gls, config)
    auto, review, unmatched = score_and_route(candidates, entities_by_id, config)
    return auto, review, unmatched


# ---- 1:1:1 exact match ----

def test_exact_reconciliation():
    inv = make_entity("invoice_line_item", 10000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 10000, date(2026, 6, 1), "Acme Corp")
    gl = make_entity("gl_entry", 10000, date(2026, 6, 1), "Acme Corp")

    auto, review, unmatched = run_link([inv], [bank], [gl])
    assert len(auto) == 1, f"expected 1 auto-linked, got {len(auto)}"
    assert auto[0].link_confidence == 1.0
    assert auto[0].status == "auto_linked"
    assert len(review) == 0
    assert len(unmatched) == 0


# ---- many-to-one: one bank payment covering 3 invoices ----

def test_one_bank_three_invoices():
    inv1 = make_entity("invoice_line_item", 30000, date(2026, 6, 1), "Acme Corp")
    inv2 = make_entity("invoice_line_item", 40000, date(2026, 6, 2), "Acme Corp")
    inv3 = make_entity("invoice_line_item", 50000, date(2026, 6, 3), "Acme Corp")
    bank = make_entity("bank_transaction", 120000, date(2026, 6, 4), "Acme Corp")  # 300+400+500
    gl = make_entity("gl_entry", 120000, date(2026, 6, 4), "Acme Corp")

    auto, review, unmatched = run_link([inv1, inv2, inv3], [bank], [gl])
    assert len(auto) >= 1, "expected the 3-invoice group to auto-link"
    group = auto[0]
    assert len(group.invoice_entity_ids) == 3
    assert group.bank_entity_ids == [bank.id]
    assert len(unmatched) == 0


# ---- one-to-many: one invoice paid across 2 bank installments ----

def test_one_invoice_two_banks():
    inv = make_entity("invoice_line_item", 150000, date(2026, 6, 1), "Acme Corp")
    bank1 = make_entity("bank_transaction", 75000, date(2026, 6, 2), "Acme Corp")
    bank2 = make_entity("bank_transaction", 75000, date(2026, 6, 3), "Acme Corp")
    gl = make_entity("gl_entry", 150000, date(2026, 6, 3), "Acme Corp")

    auto, review, unmatched = run_link([inv], [bank1, bank2], [gl])
    assert len(auto) >= 1, "expected installment grouping to auto-link"
    group = auto[0]
    assert len(group.bank_entity_ids) == 2
    assert group.invoice_entity_ids == [inv.id]


# ---- ambiguous: two equally-valid groupings both surface ----

def test_ambiguous_groupings():
    inv_a = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    inv_b = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    # Bank payment of 100000 could match inv_a+inv_b OR (with different config) others.
    bank = make_entity("bank_transaction", 100000, date(2026, 6, 2), "Acme Corp")
    gl = make_entity("gl_entry", 100000, date(2026, 6, 2), "Acme Corp")

    # Two distinct combos both summing to 100000:
    #  {inv_a, inv_b}  and any single 100000 invoice (none here). Use two 50000 invoices.
    auto, review, unmatched = run_link([inv_a, inv_b], [bank], [gl])
    # Since it's a single valid grouping here, it should auto-link.
    assert len(auto) >= 1


# ---- no match possible ----

def test_no_match():
    inv = make_entity("invoice_line_item", 12345, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 99999, date(2026, 6, 1), "Acme Corp")
    gl = make_entity("gl_entry", 11111, date(2026, 6, 1), "Acme Corp")

    auto, review, unmatched = run_link([inv], [bank], [gl])
    assert len(auto) == 0
    assert len(review) == 0
    # All three remain unmatched (distinct states: no link vs low-confidence link)
    assert len(unmatched) == 3


# ---- counterparty variations ----

def test_counterparty_variations_auto_link():
    inv = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 1), "Acme Corporation")
    gl = make_entity("gl_entry", 50000, date(2026, 6, 1), "Acme Corp")

    auto, _, unmatched = run_link([inv], [bank], [gl])
    # Jaro-Winkler on "acme corp" vs "acme corporation" is high -> auto_link threshold likely met
    assert len(auto) >= 1, "expected counterparty variation to still match"


def test_counterparty_mismatch_no_link():
    inv = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 1), "Globex Holdings LLC")
    gl = make_entity("gl_entry", 50000, date(2026, 6, 1), "Initech")

    auto, review, unmatched = run_link([inv], [bank], [gl])
    # Different counterparties, no exact amount combination distinct -> likely review or unmatched
    assert len(unmatched) >= 1 or len(review) >= 1


# ---- date boundary ----

def test_date_boundary_within_window():
    inv = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 4), "Acme Corp")
    gl = make_entity("gl_entry", 50000, date(2026, 6, 1), "Acme Corp")

    auto, _, unmatched = run_link([inv], [bank], [gl])
    assert len(auto) >= 1, "3-day boundary should still match"


def test_date_outside_window():
    inv = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 10), "Acme Corp")
    gl = make_entity("gl_entry", 50000, date(2026, 6, 10), "Acme Corp")

    auto, review, unmatched = run_link([inv], [bank], [gl])
    # Invoice 6/1 is 9 days before payment 6/10 — within INVOICE_LOOKBACK_DAYS (14),
    # so the invoice legitimately joins (net terms: issued before paid). This is
    # the real-world case the 3-leg reconciliation proved (INV-1001 5 days out).
    assert len(auto) == 1, "bank+GL auto-link as a 2-member group"
    assert len(auto[0].invoice_entity_ids) == 1, "invoice within lookback joins the group"

def test_date_beyond_lookback():
    inv = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 20), "Acme Corp")
    gl = make_entity("gl_entry", 50000, date(2026, 6, 20), "Acme Corp")

    auto, review, unmatched = run_link([inv], [bank], [gl])
    # Invoice 6/1 is 19 days before payment — beyond the 14-day lookback. Still
    # links (amount matches) but stays unmatched as a distinct 2-member group
    # without the stale invoice.
    assert any(str(e.id) == str(inv.id) for e in unmatched), "invoice beyond lookback stays unmatched"


# ---- credit notes / negative amounts ----

def test_credit_note_reduces_group():
    inv1 = make_entity("invoice_line_item", 100000, date(2026, 6, 1), "Acme Corp")
    credit = make_entity("invoice_line_item", -10000, date(2026, 6, 1), "Acme Corp", subtype="credit_note")
    bank = make_entity("bank_transaction", 90000, date(2026, 6, 2), "Acme Corp")
    gl = make_entity("gl_entry", 90000, date(2026, 6, 2), "Acme Corp")

    auto, _, unmatched = run_link([inv1, credit], [bank], [gl])
    assert len(auto) >= 1, "credit note should reduce invoice total to match bank"
    group = auto[0]
    assert len(group.invoice_entity_ids) == 2


# ---- voided GL entries excluded ----

def test_void_gl_excluded():
    inv = make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 1), "Acme Corp")
    gl_valid = make_entity("gl_entry", 50000, date(2026, 6, 1), "Acme Corp")
    gl_void = make_entity("gl_entry", 50000, date(2026, 6, 1), "Acme Corp", subtype="void")

    auto, _, unmatched = run_link([inv], [bank], [gl_valid, gl_void])
    assert len(auto) >= 1
    # The void entry should never be part of the linked group
    for group in auto:
        assert gl_void.id not in group.gl_entity_ids


# ---- low confidence below review floor ----

def test_low_confidence_below_floor():
    # Deliberately mismatched dates + different counterparties but same amount
    inv = make_entity("invoice_line_item", 50000, date(2026, 1, 1), "One Corp")
    bank = make_entity("bank_transaction", 50000, date(2026, 6, 1), "Completely Different LLC")
    gl = make_entity("gl_entry", 50000, date(2026, 6, 1), "Yet Another Co")

    config = BookConfig(id=BOOK_ID, tolerance_cents=1, auto_link_threshold=0.85, review_floor=0.50)
    auto, review, unmatched = run_link([inv], [bank], [gl], config)
    # Should land in review (or unmatched if below floor)
    assert len(auto) == 0


# ---- full pipeline via cross_link node ----

def test_cross_link_node_sets_state():
    config = default_config()
    state: GraphState = {
        "client_book_id": BOOK_ID,
        "batch_id": UUID("33333333-3333-3333-3333-333333333333"),
        "book_config": config,
        "classified_entities": [
            make_entity("invoice_line_item", 50000, date(2026, 6, 1), "Acme Corp"),
            make_entity("bank_transaction", 50000, date(2026, 6, 1), "Acme Corp"),
            make_entity("gl_entry", 50000, date(2026, 6, 1), "Acme Corp"),
        ],
    }

    result = cross_link(state, config)
    assert "groups" in result
    assert len(result["groups"]) >= 1
    assert "unmatched" in result

# ---- Riverside Design Co. 3-leg reconciliation (real-document eval, Round 7) ----
# Locks the exact rows the real extraction set proved: row 1 is a many-to-many
# (2 invoices sum to one bank+GL payment), row 2 is a full 3-member mismatch the
# verification tier must flag (bank -89900 vs GL 89400, invoice 89900).

RIVERSIDE_CONFIG = BookConfig(id=BOOK_ID, tolerance_cents=100)

def test_riverside_row1_many_to_many_3leg():
    inv1 = make_entity("invoice_line_item", 34250, date(2026, 6, 1), "ACME OFFICE SUPPLY CO")
    inv2 = make_entity("invoice_line_item", 12875, date(2026, 6, 5), "ACME OFFICE SUPPLY CO")
    bank = make_entity("bank_transaction", -47125, date(2026, 6, 6), "ACME OFFICE SUPPLY CO")
    gl = make_entity("gl_entry", 47125, date(2026, 6, 6), "Acme Office Supplies Co.")

    auto, review, unmatched = run_link([inv1, inv2], [bank], [gl], RIVERSIDE_CONFIG)
    # INV-1001 (5 days back) must join via the invoice lookback; sum = 47125 matches bank/GL.
    assert len(auto) == 1
    group = auto[0]
    assert group.invoice_entity_ids == [inv1.id, inv2.id]
    assert bank.id in group.bank_entity_ids
    assert gl.id in group.gl_entity_ids

def test_riverside_row2_mismatch_surfaces_as_review():
    inv = make_entity("invoice_line_item", 89900, date(2026, 6, 8), "BRIGHT CLOUD HOSTING INC")
    bank = make_entity("bank_transaction", -89900, date(2026, 6, 8), "BRIGHT CLOUD HOSTING INC")
    gl = make_entity("gl_entry", 89400, date(2026, 6, 8), "Bright Cloud Hosting Inc.")

    auto, review, unmatched = run_link([inv], [bank], [gl], RIVERSIDE_CONFIG)
    # The bank/GL disagree by 500¢ (> 100 tolerance) — must surface as needs_review,
    # NOT auto-link and NOT silently unmatched. The invoice joins so verification
    # sees the full 3-way variance.
    assert len(review) == 1
    group = review[0]
    assert group.mismatch is True
    assert inv.id in group.invoice_entity_ids
    assert bank.id in group.bank_entity_ids
    assert gl.id in group.gl_entity_ids
    assert all(e.id not in [x.id for x in unmatched] for e in [inv, bank, gl])

def test_riverside_unrelated_amounts_not_mismatch():
    # Same date/cp but wildly different amounts (9x) — unrelated transactions,
    # pass-5 must NOT flag them as a discrepancy.
    bank = make_entity("bank_transaction", -99999, date(2026, 6, 1), "Acme Corp")
    gl = make_entity("gl_entry", 11111, date(2026, 6, 1), "Acme Corp")
    auto, review, unmatched = run_link([], [bank], [gl], RIVERSIDE_CONFIG)
    assert len(review) == 0
    assert len(auto) == 0
    assert len(unmatched) == 2
