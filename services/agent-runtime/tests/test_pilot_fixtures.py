# services/agent-runtime/tests/test_pilot_fixtures.py
# Pilot validation harness — runs the synthetic fixtures from
# services/ingestion/test_fixtures/ through the real cross-linking algorithm
# (doc 06 §8). Offline: no LLM, no network, no DB.

import csv
import os
import re
import sys
from datetime import date
from uuid import UUID

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, BookConfig
from graph.link import build_candidate_groups, score_and_route

BOOK_ID = UUID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
DOC_ID = UUID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

FIXTURES_DIR = os.path.join(os.path.dirname(__file__), "..", "..", "ingestion", "test_fixtures")


# ---- entity construction helpers (mirrors tests/test_link.py) ----

def make_entity(
    etype: str,
    amount_cents: int,
    txn_date: date,
    counterparty: str,
    desc: str,
    doc_id: UUID = DOC_ID,
) -> ExtractedEntity:
    return ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=doc_id,
        entity_type=etype,
        amount_cents=amount_cents,
        transaction_date=txn_date,
        counterparty=counterparty,
        description=desc,
        source_format="structured",
    )


# ---- fixture parsers ----

def parse_invoice_csv(path: str) -> list:
    """Parse the simple date,amount,description,counterparty,currency CSV."""
    out = []
    with open(path) as f:
        for row in csv.DictReader(f):
            row = {k.strip().lower(): v.strip() for k, v in row.items()}
            if not row.get("date"):
                continue
            cents = _money_to_cents(row["amount"])
            out.append((
                date.fromisoformat(row["date"]),
                cents,
                row.get("counterparty", ""),
                row.get("description", ""),
            ))
    return out


def parse_gl_csv(path: str) -> list:
    """Parse a QuickBooks-style GL export: Transaction Date, Num, Name, Memo,
    Account, Debit, Credit. Signed amount = credit - debit (negative for
    refunds/debits, positive for income credits)."""
    out = []
    with open(path) as f:
        for row in csv.DictReader(f):
            row = {k.strip().lower(): v.strip() for k, v in row.items()}
            if not row.get("transaction date"):
                continue
            debit = _money_to_cents(row.get("debit", "0"))
            credit = _money_to_cents(row.get("credit", "0"))
            cents = credit - debit
            out.append((
                date.fromisoformat(row["transaction date"]),
                cents,
                row.get("name", ""),
                row.get("memo", ""),
            ))
    return out


def parse_ofx(path: str) -> list:
    """Parse OFX <STMTTRN> blocks: DTPOSTED, TRNAMT, NAME, MEMO."""
    text = open(path).read()
    blocks = re.findall(r"<STMTTRN>(.*?)</STMTTRN>", text, re.DOTALL)
    out = []
    for b in blocks:
        posted = re.search(r"<DTPOSTED>(\d{8})", b)
        amt = re.search(r"<TRNAMT>(-?\d+\.?\d*)</TRNAMT>", b)
        name = re.search(r"<NAME>(.*?)</NAME>", b)
        memo = re.search(r"<MEMO>(.*?)</MEMO>", b)
        if not posted or not amt:
            continue
        # The link algorithm compares magnitudes (test_link.py uses positive
        # cents); OFX debits carry a negative TRNAMT, so normalize to abs.
        cents = abs(_money_to_cents(amt.group(1)))
        out.append((
            date.fromisoformat(posted.group(1)),
            cents,
            name.group(1) if name else "",
            memo.group(1) if memo else "",
        ))
    return out


def _money_to_cents(v: str) -> int:
    """'1234.56' -> 123456; '-1234.56' -> -123456."""
    v = v.strip().replace(",", "")
    negative = v.startswith("-")
    if negative:
        v = v[1:]
    if "." in v:
        whole, frac = v.split(".", 1)
        frac = (frac + "00")[:2]
    else:
        whole, frac = v, "00"
    cents = int(whole) * 100 + int(frac)
    if negative:
        cents = -cents
    return cents


def entities_from_fixtures(invoice_path, bank_path, gl_path):
    """Build invoice/bank/gl ExtractedEntity lists from the fixture files.

    Negative-amount rows (credit notes/refunds) get entity_subtype="credit_note"
    so the link algorithm groups signed negatives correctly (mirrors how the
    classifier flags them downstream).
    """
    invoices, banks, gls = [], [], []
    for txn_date, cents, cp, desc in parse_invoice_csv(invoice_path):
        e = make_entity("invoice_line_item", cents, txn_date, cp, desc)
        if cents < 0:
            e.entity_subtype = "credit_note"
        invoices.append(e)
    for txn_date, cents, cp, desc in parse_ofx(bank_path):
        e = make_entity("bank_transaction", cents, txn_date, cp, desc)
        # OFX expresses the refund as a positive CREDIT while the invoice/GL
        # express it as a negative refund. Align to the invoice sign convention.
        if cents > 0 and "refund" in desc.lower():
            e.amount_cents = -cents
            e.entity_subtype = "credit_note"
        banks.append(e)
    for txn_date, cents, cp, desc in parse_gl_csv(gl_path):
        e = make_entity("gl_entry", cents, txn_date, cp, desc)
        if cents < 0:
            e.entity_subtype = "credit_note"
        gls.append(e)
    return invoices, banks, gls


# ---- tests ----

def test_sample_invoice_bank_ofx_produces_valid_reconciliation_group():
    inv_csv = os.path.join(FIXTURES_DIR, "sample_invoice.csv")
    bank_ofx = os.path.join(FIXTURES_DIR, "sample_bank.ofx")
    gl_csv = os.path.join(FIXTURES_DIR, "sample_gl.csv")
    assert os.path.exists(inv_csv)
    assert os.path.exists(bank_ofx)
    assert os.path.exists(gl_csv)

    invoices, banks, gls = entities_from_fixtures(inv_csv, bank_ofx, gl_csv)
    assert len(invoices) == 10, f"expected 10 invoice lines, got {len(invoices)}"
    assert len(banks) == 10, f"expected 10 bank transactions, got {len(banks)}"
    assert len(gls) == 10, f"expected 10 gl entries, got {len(gls)}"

    config = BookConfig(id=BOOK_ID)
    entities = invoices + banks + gls
    entities_by_id = {str(e.id): e for e in entities}

    candidates = build_candidate_groups(invoices, banks, gls, config)
    assert candidates, "sample invoice + bank ofx + gl must produce candidate groups"

    auto, review, unmatched = score_and_route(candidates, entities_by_id, config)
    assert auto, "clean fixture pair must auto-link at least one group"
    # Every fixture row should resolve into an auto-linked or review group.
    assert len(unmatched) == 0, f"clean fixtures left {len(unmatched)} entities unmatched"

    for group in auto:
        assert group.status == "auto_linked"
        assert group.invoice_entity_ids and group.bank_entity_ids and group.gl_entity_ids
        assert group.link_confidence >= config.auto_link_threshold


def test_mismatch_invoice_does_not_auto_link():
    inv_csv = os.path.join(FIXTURES_DIR, "sample_mismatch_invoice.csv")
    bank_ofx = os.path.join(FIXTURES_DIR, "sample_bank.ofx")
    gl_csv = os.path.join(FIXTURES_DIR, "sample_gl.csv")
    assert os.path.exists(inv_csv)

    invoices, banks, gls = entities_from_fixtures(inv_csv, bank_ofx, gl_csv)
    config = BookConfig(id=BOOK_ID)
    entities = invoices + banks + gls
    entities_by_id = {str(e.id): e for e in entities}

    candidates = build_candidate_groups(invoices, banks, gls, config)
    auto, review, unmatched = score_and_route(candidates, entities_by_id, config)

    # The planted $250 overstatement (vs 1500 in bank/gl) must NOT auto-link its
    # invoice. Bank+GL 2-member groups legitimately auto-link (matching amounts);
    # the per-invoice assertion below is the real check.

    # Every one of the 3 mismatch invoices must land in review/unmatched, never
    # auto-linked. (The 20 clean bank/gl entities may still end up unmatched —
    # they have no matching invoice here, so we only scope the assertion to the
    # mismatch invoices.)
    mismatched_ids = {str(e.id) for e in invoices}
    linked_ids = set()
    for g in auto + review:
        linked_ids.update(str(i) for i in g.invoice_entity_ids)
    missing = mismatched_ids - linked_ids
    assert len(missing) == 3, (
        f"all 3 mismatch invoices must go to review/unmatched; "
        f"{len(missing)} of them auto-linked or near-matched"
    )
