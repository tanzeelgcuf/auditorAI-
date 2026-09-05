"""HARNESS-ONLY exact mirror of services/ingestion/src/ocr/structured.rs::parse_amount.

READ THIS BEFORE IMPORTING IT ANYWHERE.

Production money parsing lives in Rust. This file exists so the Python-side
harnesses (run_extraction.py against real docTR output, tests/test_pilot_fixtures.py
against the CSV/OFX fixtures) can turn a raw string into cents WITHOUT any of
them growing its own parser. Before this module there were three of them —
graph/extract.py::_parse_amount_cents (deleted), run_extraction.py::_raw_to_cents
and tests/test_pilot_fixtures.py::_money_to_cents — and they disagreed with each
other and with Rust:

  - a bare integer "1250" meant $12.50 in Python and $1,250.00 in Rust (100x),
  - "(45.00)" came back POSITIVE (accounting negatives inverted),
  - "1.500,00" came back as 150 cents (1000x understated),
  - unparseable input returned 0, so a garbled amount became a $0.00 entity that
    reconciles with zero variance instead of failing the document.

Rules this module follows, and the reason for each:

  1. NO BINARY FLOAT. `Decimal` only. `float("1.15") * 100` is
     114.99999999999999 and `float("1.005") * 100` is 100.49999999999999
     (measured in this interpreter, not assumed).
  2. AMBIGUITY RETURNS None, NEVER 0. "1.500" could be $1.50 or EUR 1,500;
     refusing it makes the caller fail the row. A 0 would silently report a
     $0.00 total as if it had been read successfully.
  3. NO ROUNDING. Rounding money is itself a calculation. A third decimal place
     is refused rather than truncated or rounded to cents.
  4. structured.rs IS THE CONTRACT. If this file and the Rust parser ever
     disagree, the Rust side is right and this file is the bug. Its AMOUNT_CASES
     table and tests/test_amount_mirror.py::CASES are kept in step deliberately.

NOT FOR PRODUCTION USE. Nothing under graph/, mcp_client/ or main.py may import
this module — tests/test_amount_mirror.py::test_no_production_module_imports_the_mirror
enforces that mechanically, because a harness parser that leaks into the service
would put money arithmetic back into Python.
"""

from decimal import Decimal, InvalidOperation
from typing import Optional

# Characters that may appear in an amount but are not part of its value:
# whitespace (incl. NBSP, written \xa0 so it is visible in a diff), the
# apostrophe used as a Swiss thousands separator, and currency symbols.
SKIPPABLE = set(" \t\r\n\xa0'") | set("$€£¥₹¢₩₽₺₴₦₱₡₪¤﷼")


def _admissible(ch: str) -> bool:
    return (ch.isascii() and ch.isdigit()) or ch in ".," or ch in SKIPPABLE


def parse_amount_cents(value) -> Optional[int]:
    """Return integer cents, or None when `value` is not an unambiguous amount.

    A bare integer is DOLLARS ("100" -> 10000), matching structured.rs.
    """
    if value is None:
        return None
    s = str(value).strip()
    if not s:
        return None

    # --- sign: parens, leading '-', trailing '-' (SAP/mainframe exports) ---
    neg = False
    if s.startswith("(") and s.endswith(")") and len(s) >= 2:
        neg, s = True, s[1:-1].strip()
    elif s.startswith("-"):
        neg, s = True, s[1:].strip()
    elif s.endswith("-"):
        neg, s = True, s[:-1].strip()
    elif s.startswith("+"):
        s = s[1:].strip()

    # --- accept-list gate (mirrors structured.rs::keep_numeric) ---
    # Anything not admissible REFUSES the string rather than being filtered out,
    # so "45.00 CR" (a credit convention), "45.00%" and "4/5" cannot become 4500.
    if not all(_admissible(c) for c in s):
        return None

    body = "".join(c for c in s if (c.isascii() and c.isdigit()) or c in ".,")
    if not body:
        return None

    # --- which separator is the decimal point? ---
    dots, commas = body.count("."), body.count(",")
    if dots and commas:
        dec = "." if body.rfind(".") > body.rfind(",") else ","
    elif not dots and not commas:
        dec = None
    else:
        sep, n = (".", dots) if dots else (",", commas)
        if n > 1:
            dec = None  # "1.234.567" -> both occurrences are thousands separators
        elif len(body.rsplit(sep, 1)[-1]) in (1, 2):
            dec = sep
        else:
            return None  # "1.500" (unknowable) / "1234.567" (not cents)

    int_raw, frac = (body.rsplit(dec, 1) if dec else (body, ""))

    # --- the other separator must group in exact thousands ---
    thou = "," if dec == "." else "." if dec == "," else ("." if dots else ",")
    groups = int_raw.split(thou)
    if len(groups) > 1:
        if not groups[0] or len(groups[0]) > 3:
            return None
        if any(len(g) != 3 for g in groups[1:]):
            return None  # "12,34,567.89" (Indian lakh) is unsupported: refuse

    digits = "".join(c for c in int_raw if c.isdigit())
    if not digits and not frac:
        return None
    if frac and not frac.isdigit():
        return None
    if len(frac) > 2:
        return None  # unreachable via the paths above; never round to fit

    canonical = f"{digits or '0'}.{(frac + '00')[:2]}" if frac else digits
    if not canonical:
        return None
    try:
        cents = int(Decimal(canonical) * 100)
    except (InvalidOperation, ValueError):
        return None
    return -cents if neg else cents
