#!/usr/bin/env python3
"""Cross-language drift guard for the money parser.

services/ingestion/src/ocr/structured.rs::parse_amount is the production money
parser and the CONTRACT. services/agent-runtime/harness_amount_mirror.py is a
Python mirror used by the offline harnesses. A mirror that drifts is worse than
no mirror: the last time these two disagreed, the string "1250" meant $12.50 in
Python and $1,250.00 in Rust, and BOTH test suites were green.

This script:
  1. parses the AMOUNT_CASES table out of structured.rs (the Rust table is the
     source of truth — this direction is deliberate),
  2. runs the Python mirror against every one of those cases,
  3. reports any case the Rust table asserts that the Python table omits.

It does NOT run the Rust. `cargo test` is still required to prove the Rust side
behaves the way its own table claims; this only proves the mirror agrees with
what that table asserts.

Exit code 0 = agree, 1 = drift.
"""

import argparse
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUST = os.path.join(ROOT, "services", "ingestion", "src", "ocr", "structured.rs")
MIRROR_DIR = os.path.join(ROOT, "services", "agent-runtime")
PY_TABLE = os.path.join(MIRROR_DIR, "tests", "test_amount_mirror.py")

# ("input", Some(12345), "why")  |  ("input", None, "why")
ROW = re.compile(
    r'^\s*\("((?:[^"\\]|\\.)*)"\s*,\s*(?:Some\((-?\d+)\)|None)\s*,\s*"',
)


def _unescape(literal):
    """Decode the escapes a Rust string literal in this table can contain.

    Deliberately NOT `.encode().decode("unicode_escape")`: that round-trips
    through latin-1 and turned "€89.99" into "â\\x82¬89.99", which then failed
    against a mirror that handles the euro sign correctly — a guard reporting a
    bug in itself as a bug in the code under test.
    """
    out = []
    i = 0
    pairs = {"n": "\n", "t": "\t", "r": "\r", '"': '"', "\\": "\\", "'": "'", "0": "\0"}
    while i < len(literal):
        ch = literal[i]
        if ch == "\\" and i + 1 < len(literal) and literal[i + 1] in pairs:
            out.append(pairs[literal[i + 1]])
            i += 2
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def rust_cases(path):
    text = open(path, encoding="utf-8").read()
    start = text.index("const AMOUNT_CASES")
    end = text.index("];", start)
    cases = []
    for line in text[start:end].splitlines():
        m = ROW.match(line)
        if not m:
            continue
        cases.append((_unescape(m.group(1)),
                      int(m.group(2)) if m.group(2) is not None else None))
    return cases


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    sys.path.insert(0, MIRROR_DIR)
    from harness_amount_mirror import parse_amount_cents

    cases = rust_cases(RUST)
    if len(cases) < 20:
        print(f"FAIL: parsed only {len(cases)} rows out of AMOUNT_CASES — the "
              f"table format changed and this guard stopped guarding anything")
        return 1

    failures = []
    for raw, expect in cases:
        got = parse_amount_cents(raw)
        if got != expect:
            failures.append(f"  {raw!r:20} rust={expect!r:>16}  python={got!r}")
        elif args.verbose:
            print(f"  ok {raw!r:20} -> {got!r}")

    py_text = open(PY_TABLE, encoding="utf-8").read()
    missing = [raw for raw, _ in cases if f'("{raw}"' not in py_text
               and raw not in ("",)]

    print(f"{len(cases) - len(failures)}/{len(cases)} Rust AMOUNT_CASES rows "
          f"reproduced by the Python mirror")
    if failures:
        print("DRIFT — the mirror disagrees with the Rust contract:")
        print("\n".join(failures))
    if missing:
        print(f"NOTE: {len(missing)} Rust case(s) are not also pinned in "
              f"tests/test_amount_mirror.py: {missing}")
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
