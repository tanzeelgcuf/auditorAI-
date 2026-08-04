#!/usr/bin/env python3
"""Generate the Prompt B stress-test document set.

Deliberately unlike the clean Riverside set:
- Two invoices, SAME $150.00 amount, from confusably-similar vendors:
  SLG-771 "SLG Landscaping Group" (invoice INV-771) and
  SLM-9042 "SLM Lawn Maintenance" (invoice INV-9042).
  The linker must disambiguate by counterparty, not just amount.
- Page 2 (INV-9042) is ROTATED 15deg — simulates a scan-skewed page to stress
  OCR beyond clean digital text.
- Bank statement has TXN20580 with NO invoice or GL leg — must stay unmatched.
- GL export uses DIFFERENT column headers than Riverside (not date/amount/
  account — but posting_date, credit_amount, debit_amount, gl_code) to test
  the column-mapping logic's generality.

Files (written to /Users/apple/Downloads):
  stress_invoices_july2026.pdf    (4-page invoice batch)
  stress_bank_statement_july2026.ofx
  stress_gl_export_july2026.csv
Plus STRESS_TEST_README.md next to them with the pass/fail bar.
"""
import os
import textwrap
from reportlab.lib.pagesizes import letter
from reportlab.pdfgen import canvas

OUT = os.path.expanduser("~/Downloads")

# --- Invoice page content ---

def _invoice_page(c, vendor, vendor_addr, invoice_no, amount, date, rotated=False):
    w, h = letter
    if rotated:
        c.saveState()
        c.translate(w / 2, h / 2)
        c.rotate(15)  # scan skew
        c.translate(-w / 2, -h / 2)
    c.setFont("Helvetica-Bold", 14)
    c.drawString(72, h - 72, vendor)
    c.setFont("Helvetica", 10)
    for i, line in enumerate(vendor_addr.split("\n")):
        c.drawString(72, h - 90 - i * 14, line)
    c.setFont("Helvetica-Bold", 11)
    c.drawString(72, h - 160, f"Invoice #{invoice_no}")
    c.drawString(400, h - 160, f"Date: {date}")
    # line items
    c.setFont("Helvetica", 10)
    c.drawString(72, h - 200, "Description")
    c.drawString(450, h - 200, "Amount")
    c.line(72, h - 208, 540, h - 208)
    items = [
        ("Landscaping service (week of Jun 8)", "120.00"),
        ("Materials", "30.00"),
    ]
    y = h - 224
    for desc, amt in items:
        c.drawString(72, y, desc)
        c.drawString(460, y, amt)
        y -= 20
    c.line(72, y + 6, 540, y + 6)
    c.setFont("Helvetica-Bold", 10)
    c.drawString(72, y - 14, "TOTAL")
    c.drawString(450, y - 14, f"${amount}")
    c.drawString(72, y - 40, f"Net 15 days")
    if rotated:
        c.restoreState()


def gen_invoices_pdf(path):
    c = canvas.Canvas(path, pagesize=letter)
    # Page 1: SLG-771 (clean)
    _invoice_page(
        c, "SLG Landscaping Group", "4300 Frontage Rd\nAustin, TX 78701\n(512) 555-0171",
        "INV-771", "150.00", "2026-07-01", rotated=False)
    c.showPage()
    # Page 2: SLM-9042 (ROTATED)
    _invoice_page(
        c, "SLM Lawn Maintenance", "4422 Congress Ave\nAustin, TX 78704\n(512) 555-0234",
        "INV-9042", "150.00", "2026-07-03", rotated=True)
    c.showPage()
    # Page 3: invoice for a different amount (unmatched reference, no bank leg)
    _invoice_page(
        c, "Blue River Paving Co.", "900 Cirrus Park Drive\nAustin, TX 78727\n(512) 555-0310",
        "INV-8005", "75.00", "2026-07-05", rotated=False)
    c.showPage()
    c.save()
    print(f"wrote {path}")


def gen_ofx(path):
    ofx = textwrap.dedent("""\
        OFXHEADER:100
        DATA:OFXSGML
        VERSION:102
        SECURITY:NONE
        ENCODING:USASCII
        CHARSET:1252
        COMPRESSION:NONE
        OLDFILEUID:NONE
        NEWFILEUID:NONE

        <OFX>
        <SIGNONMSGSRSV1>
        <SONRS>
        <STATUS><CODE>0</CODE><SEVERITY>INFO</SEVERITY></STATUS>
        <DTSERVER>20260731000000</DTSERVER>
        <LANGUAGE>ENG</LANGUAGE>
        </SONRS>
        </SIGNONMSGSRSV1>
        <BANKMSGSRSV1>
        <STMTTRNRS>
        <TRNUID>1</TRNUID>
        <STATUS><CODE>0</CODE><SEVERITY>INFO</SEVERITY></STATUS>
        <STMTRS>
        <CURDEF>USD</CURDEF>
        <BANKACCTFROM><BANKID>114000545</BANKID><ACCTID>245678901</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM>
        <BANKTRANLIST>
        <DTSTART>20260701</DTSTART>
        <DTEND>20260731</DTEND>
        <STMTTRN>
          <TRNTYPE>DEBIT</TRNTYPE>
          <DTPOSTED>20260705</DTPOSTED>
          <TRNAMT>-150.00</TRNAMT>
          <FITID>20551</FITID>
          <NAME>SLG LANDSCAPING GROUP</NAME>
          <MEMO>TXN20551 ACH DEBIT</MEMO>
        </STMTTRN>
        <STMTTRN>
          <TRNTYPE>DEBIT</TRNTYPE>
          <DTPOSTED>20260706</DTPOSTED>
          <TRNAMT>-150.00</TRNAMT>
          <FITID>20567</FITID>
          <NAME>SLM LAWN MAINTENANCE</NAME>
          <MEMO>TXN20567 ACH DEBIT</MEMO>
        </STMTTRN>
        <STMTTRN>
          <TRNTYPE>DEBIT</TRNTYPE>
          <DTPOSTED>20260712</DTPOSTED>
          <TRNAMT>-45.75</TRNAMT>
          <FITID>20580</FITID>
          <NAME>OFFICE DEPOT</NAME>
          <MEMO>TXN20580 POS DEBIT</MEMO>
        </STMTTRN>
        </BANKTRANLIST>
        </STMTRS>
        </STMTTRNRS>
        </BANKMSGSRSV1>
        </OFX>
    """)
    with open(path, "w") as f:
        f.write(ofx)
    print(f"wrote {path}")


def gen_gl_csv(path):
    # Deliberately different headers than Riverside (which used date,amount,account).
    # Uses posting_date, debit_amount, credit_amount, gl_code.
    csv = textwrap.dedent("""\
        posting_date,debit_amount,credit_amount,gl_code,description
        2026-07-05,150.00,,1101,SLG Landscaping Group invoice INV-771
        2026-07-06,150.00,,1101,SLM Lawn Maintenance invoice INV-9042
        2026-07-06,,150.00,1101,Payment - INV-771
        2026-07-08,,150.00,1101,Payment - INV-9042
    """)
    with open(path, "w") as f:
        f.write(csv)
    print(f"wrote {path}")


def gen_readme(path):
    readme = textwrap.dedent("""\
        # STRESS TEST — pass/fail bar (doc 14 / Prompt B)

        Purpose: verify the live pipeline against deliberate ambiguity that the
        clean Riverside set couldn't produce.

        ## Expected results

        1. TXN20551 (bank $150, "SLG LANDSCAPING GROUP") links to SLG-771 (INV-771).
        2. TXN20567 (bank $150, "SLM LAWN MAINTENANCE") links to SLM-9042 (INV-9042).
           - Correct = no cross-link. Acceptable = either routes to needs_review
             (safer than a wrong auto-link). FAIL = cross-linked or one swallowed.
        3. TXN20580 (bank $45.75 "OFFICE DEPOT") stays UNMATCHED — no invoice, no GL.
           Check explicitly.
        4. Page 2 (rotated INV-9042) — extraction accuracy of vendor, amount, date.
           Report specific errors; don't average into an aggregate.
        5. GL CSV different headers (posting_date/debit_amount/credit_amount/gl_code)
           must trigger the column-mapping screen and map to the right fields.
        6. INV-8005 ($75 Blue River Paving) has a bank leg? No — it's invoice-only,
           so it should be unmatched/incomplete, not falsely auto-linked.

        ## Wiring-first scrutiny
        Any "model accuracy" or "expected degradation" result must be checked for a
        wiring cause first (ordering bug, Riverside-specific hardcode, rotation
        handling) before accepting it as a capability limit.
    """)
    with open(path, "w") as f:
        f.write(readme)
    print(f"wrote {path}")


def main():
    gen_invoices_pdf(os.path.join(OUT, "stress_invoices_july2026.pdf"))
    gen_ofx(os.path.join(OUT, "stress_bank_statement_july2026.ofx"))
    gen_gl_csv(os.path.join(OUT, "stress_gl_export_july2026.csv"))
    gen_readme(os.path.join(OUT, "STRESS_TEST_README.md"))


if __name__ == "__main__":
    main()
