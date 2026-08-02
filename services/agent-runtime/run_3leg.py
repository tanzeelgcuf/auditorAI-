"""Round-7 3-leg reconciliation proof: 4 invoices + 5 bank + 5 GL -> groups.

Loads real bank/GL entities from DB (Riverside synthetic pair), injects the 4
corrected invoice entities (from the raw-string extraction contract eval set),
and runs the linker to prove row 1 (2-invoice many-to-many) and row 2 (3-member
high finding) form full 3-leg groups — the pieces the earlier run couldn't show
because extraction amounts were wrong.

Invoice truth (doc 12 / eval fixture):
  INV-1001 $342.50 = 34250
  INV-1002 $128.75 = 12875
  BCH-2291 $899.00 = 89900
  MP-5502 $215.00 = 21500
"""
import os
import sys
from datetime import date
from uuid import UUID, uuid4

sys.path.insert(0, os.path.dirname(__file__))

from graph.schema import ExtractedEntity, BookConfig
from graph.link import cross_link

BOOK_ID = UUID("e4ef2d95-ca61-42fd-8442-a398fe54de05")
BANK_DOC = UUID("716a29cc-b482-4e8b-bf6b-a9b75d8929ce")
GL_DOC = UUID("29402356-2b9a-40fc-978a-d7a647c118ec")
PDF_DOC = UUID("da5c7778-07f3-4646-beb5-8eeda27f0788")

INVOICES = [
    # (amount_cents, invoice_number, date, vendor)
    (34250, "INV-1001", "2026-06-01", "Acme Office Supply Co"),
    (12875, "INV-1002", "2026-06-05", "Acme Office Supply Co"),
    (89900, "BCH-2291", "2026-06-08", "Bright Cloud Hosting Inc"),
    (21500, "MP-5502", "2026-06-15", "Metro Print Shop"),
]

CFG = BookConfig(
    id=BOOK_ID,
    tolerance_cents=100,
    tolerance_mode="fixed",
    auto_link_threshold=0.85,
    review_floor=0.50,
)


def main():
    import psycopg2
    conn = psycopg2.connect(os.getenv("DATABASE_URL", "dbname=ai_auditor_smoke user=apple"))
    cur = conn.cursor()

    def load(doc, etype):
        cur.execute(
            "SELECT amount_cents, transaction_date, counterparty, description, gl_account_code "
            "FROM extracted_entities WHERE source_document_id=%s AND entity_type=%s",
            (str(doc), etype),
        )
        out = []
        for amt, txndate, cp, desc, gl in cur.fetchall():
            txdate = date.fromisoformat(str(txndate)) if txndate else None
            out.append(ExtractedEntity(
                id=uuid4(), client_book_id=BOOK_ID, source_document_id=doc,
                entity_type=etype,
                amount_cents=int(amt),
                transaction_date=txdate,
                counterparty=cp, description=desc, gl_account_code=gl,
                source_format="structured",
            ))
        return out

    banks = load(BANK_DOC, "bank_transaction")
    gls = load(GL_DOC, "gl_entry")
    invoices = [
        ExtractedEntity(
            id=uuid4(), client_book_id=BOOK_ID, source_document_id=PDF_DOC,
            entity_type="invoice_line_item",
            amount_cents=amt,
            transaction_date=date.fromisoformat(d),
            counterparty=vendor,
            description=f"Invoice {num}",
            source_format="ocr",
        )
        for amt, num, d, vendor in INVOICES
    ]

    entities = invoices + banks + gls
    state = {"client_book_id": BOOK_ID, "classified_entities": entities}

    result = cross_link(state, CFG)
    groups = result.get("groups", [])
    unmatched = result.get("unmatched", [])

    print(f"\n=== GROUPS ({len(groups)}) ===")
    for g in sorted(groups, key=lambda g: -g.link_confidence):
        members = g.invoice_entity_ids + g.bank_entity_ids + g.gl_entity_ids
        invoice_idx = [str(i.id)[:8] for i in invoices if i.id in g.invoice_entity_ids]
        bank_idx = [str(b.id)[:8] for b in banks if b.id in g.bank_entity_ids]
        gl_idx = [str(g2.id)[:8] for g2 in gls if g2.id in g.gl_entity_ids]
        amt_sum = sum(e.amount_cents for e in invoices if e.id in g.invoice_entity_ids)
        bank_amt = sum(e.amount_cents for e in banks if e.id in g.bank_entity_ids)
        gl_amt = sum(e.amount_cents for e in gls if e.id in g.gl_entity_ids)
        print(f"  scope={g.status} conf={g.link_confidence:.2f} "
              f"inv[{len(invoice_idx)}]=+{amt_sum} bank[{len(bank_idx)}]={bank_amt} gl[{len(gl_idx)}]={gl_amt}")

    print(f"\n=== UNMATCHED ({len(unmatched)}) ===")
    for e in unmatched:
        print(f"  {e.entity_type} {e.description} {e.amount_cents}")

    # Row-2 assertion: bank -89900, GL +89400, invoice +89900. Difference between
    # GL and invoice = 89900-89400 = 500 > tolerance 100 -> should be high finding.
    print("\n=== ROW-2 DISCREPANCY ===")
    for g in groups:
        b = sum(e.amount_cents for e in banks if e.id in g.bank_entity_ids)
        gi = sum(e.amount_cents for e in gls if e.id in g.gl_entity_ids)
        iv = sum(e.amount_cents for e in invoices if e.id in g.invoice_entity_ids)
        if gi == 89400:
            diff_inv_gl = iv - gi
            diff_bank_gl = abs(b) - gi
            print(f"  GL=89400 inv={iv} bank={b}")
            print(f"  inv-vs-GL diff={diff_inv_gl}¢ (tolerance 100) -> {'EXCEEDS' if abs(diff_inv_gl) > 100 else 'ok'}")
            print(f"  bank-vs-GL diff={diff_bank_gl}¢")


if __name__ == "__main__":
    main()