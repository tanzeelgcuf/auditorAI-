# services/agent-runtime/tests/test_amount_mirror.py
# The in-repo guard for harness_amount_mirror.parse_amount_cents.
#
# Two things this file exists to prevent:
#
#  1. DRIFT FROM RUST. Every case below is also a row in
#     services/ingestion/src/ocr/structured.rs::AMOUNT_CASES. If the two tables
#     ever disagree, the Rust one is the contract and this file is the bug — the
#     100x "bare integer" disagreement (Python read "1250" as $12.50, Rust as
#     $1,250.00) survived for as long as it did precisely because each side had
#     its own green table and neither referenced the other.
#  2. THE MIRROR LEAKING INTO PRODUCTION. It is a harness. If graph/ or
#     mcp_client/ ever imports it, money parsing is back in Python.

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from harness_amount_mirror import parse_amount_cents

SERVICE_DIR = os.path.join(os.path.dirname(__file__), "..")

# (raw, expected_cents_or_None, why this case is in the table)
CASES = [
    # --- the pre-existing Rust unit tests ---
    ("150.00", 15000, "test_parse_amount_usd"),
    ("2500.00", 250000, "test_parse_amount_usd"),
    ("-150.00", -15000, "test_parse_amount_negative"),
    ("100", 10000, "test_parse_amount_no_decimal: a bare integer is DOLLARS"),
    ("$1,500.00", 150000, "test_parse_amount_with_currency"),
    ("€89.99", 8999, "test_parse_amount_with_currency"),
    ("471.25", 47125, "map_columns fixture in structured.rs depends on this"),
    # --- bugs the exact-decimal rewrite fixed ---
    ("(45.00)", -4500, "was +4500: accounting parens inverted the sign"),
    ("1.500,00", 150000, "was 150: European separators, 1000x understated"),
    ("45.00-", -4500, "was None->0: trailing sign (SAP/mainframe exports)"),
    ("(1,234.56)", -123456, "parens plus thousands"),
    ("1 234,56", 123456, "French: space thousands, comma decimal"),
    ("1'234.56", 123456, "Swiss apostrophe thousands"),
    # --- ambiguity is REFUSED, never guessed ---
    ("1.500", None, "$1.50 or EUR 1,500? unknowable: refuse"),
    ("1,500", None, "$1,500 or EUR 1,50? unknowable: refuse"),
    ("1234.567", None, "3 decimals cannot be cents; rounding is a calculation"),
    ("45.00 CR", None, "letters carry a sign convention: refuse, don't strip"),
    ("45.00%", None, "a percent is not an amount: refuse, don't strip"),
    ("4/5", None, "not an amount at all"),
    ("12,34,567.89", None, "Indian lakh grouping unsupported: refuse"),
]

CASES += [
    # --- garbage in, None out: callers fail the row, they never default to 0 ---
    ("", None, "empty"),
    ("-", None, "sign only"),
    (".", None, "separator only"),
    ("$", None, "symbol only"),
    ("abc", None, "not a number"),
    ("1-2", None, "trailing-minus strip leaves '1-', then '-' is inadmissible"),
    # --- exactness and boundaries ---
    ("0.01", 1, "one cent"),
    ("0.00", 0, "zero is legitimate when written explicitly"),
    ("45.5", 4550, "a single decimal digit pads to 50, it does not become 45.05"),
    (".99", 99, "leading decimal point"),
    ("1,234,567.89", 123456789, "US multi-group"),
    ("1.234.567,89", 123456789, "European multi-group"),
    ("8.65", 865, "float('8.65') * 100 is exactly 865.0 — measured, not assumed"),
    ("1.15", 115, "float('1.15') * 100 is 114.99999999999999 — the float hazard"),
    ("1.005", None, "float('1.005') * 100 is 100.49999999999999; 3dp refused anyway"),
    ("999999999999.99", 99999999999999, "large, still inside i64"),
    ("97401", 9740100, "a bare integer in a CSV amount column IS dollars"),
]

# The four pilot invoice totals from services/ingestion/test_fixtures, kept here
# because deleting graph/extract.py::_parse_amount_cents also deleted the only
# Python test that pinned them. Same values now live in the Rust AMOUNT_CASES.
PILOT_TOTALS = [("$342.50", 34250), ("$128.75", 12875),
                ("$899.00", 89900), ("$215.00", 21500)]


def test_amount_mirror_matches_the_rust_contract():
    failures = []
    for raw, expect, why in CASES:
        got = parse_amount_cents(raw)
        if got != expect:
            failures.append(f"parse_amount_cents({raw!r}) = {got!r}, want {expect!r}  ({why})")
    assert not failures, "\n".join(failures)


def test_pilot_invoice_totals_convert_exactly():
    for raw, want in PILOT_TOTALS:
        assert parse_amount_cents(raw) == want, raw


def test_bare_integer_is_dollars_not_cents():
    # The single most expensive disagreement this module was created to end.
    assert parse_amount_cents("1250") == 125000
    assert parse_amount_cents("1250") != 1250


def test_refusal_is_none_and_never_zero():
    # A returned 0 would reconcile with zero variance and look like a clean match.
    for raw in ("", "abc", "1.500", "45.00 CR", "N/A", "--", "$"):
        assert parse_amount_cents(raw) is None, raw
    # 0 is only ever returned for an amount that really is zero.
    assert parse_amount_cents("0.00") == 0


def test_no_float_in_the_mirror_source():
    # Guards the rule rather than a value: `float(` anywhere in this module means
    # binary floating point re-entered a money path.
    src = open(os.path.join(SERVICE_DIR, "harness_amount_mirror.py")).read()
    code = "\n".join(
        line for line in src.splitlines()
        if not line.lstrip().startswith("#")
    )
    body = code.split('"""', 2)[-1]  # drop the module docstring, which cites floats
    assert "float(" not in body, "binary float re-entered the money mirror"
    assert "Decimal" in body


def test_no_production_module_imports_the_mirror():
    """graph/, mcp_client/ and main.py must not import the harness mirror.

    If they do, Python is parsing money in production again — the exact
    architecture violation the mirror was extracted to make impossible.
    """
    offenders = []
    for root, dirs, files in os.walk(SERVICE_DIR):
        dirs[:] = [d for d in dirs if d not in ("tests", "__pycache__", ".git")]
        for name in files:
            if not name.endswith(".py") or name in (
                "harness_amount_mirror.py", "run_extraction.py", "run_3leg.py"
            ):
                continue
            path = os.path.join(root, name)
            if "harness_amount_mirror" in open(path).read():
                offenders.append(os.path.relpath(path, SERVICE_DIR))
    assert not offenders, (
        f"production modules import the harness money mirror: {offenders}"
    )
