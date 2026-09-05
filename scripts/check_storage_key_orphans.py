#!/usr/bin/env python3
"""Guard: a storage_key written to the database must have bytes behind it.

THE BUG THIS EXISTS TO PREVENT (found 2026-09-04, services/api/internal/
documents/documents.go): HandleUpload read the uploaded file into memory, hashed
it for duplicate detection, generated a storage_key, wrote the source_documents
row, published document.uploaded — and never wrote the bytes anywhere. The only
trace was a comment reading "In production: upload data to S3/MinIO here".

Why nothing caught it:
  - the API answered 201 Created with ocr_status "pending", so the client saw
    success;
  - services/ingestion's coordinator called StreamObject on a key that had never
    existed, got NoSuchKey, logged it, and acked the message;
  - the row stayed at ocr_status='pending' forever;
  - no test failed, because no test reached object storage.

A green suite cannot see this. The invariant has to be checked structurally: any
file that writes a storage_key into the database must also be a file that writes
the bytes, or must be explicitly allowlisted with a reason.

Exit code 0 = invariant holds, 1 = a file writes storage_key with no byte write
and no allowlist entry.
"""

import argparse
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Files permitted to write a storage_key without writing bytes, each with the
# reason it is safe. An entry here is a claim that must stay true.
ALLOWLIST = {
    "services/api/cmd/seed-demo/main.go": (
        "Demo seeder. Writes ocr_status='done' and publishes no document.uploaded "
        "event, so nothing ever streams these keys. Bytes are deliberately absent: "
        "stub files whose contents disagreed with the planted findings would make "
        "the traceability claim false during a demo. Documented at the docs slice "
        "in that file."
    ),
    "services/api/internal/middleware/security_test.go": (
        "RLS isolation fixtures. Rows exist only to be selected across tenants; "
        "no code path streams their bytes."
    ),
}

# Establishing that bytes are behind the key. EITHER writing them (the API holds
# the bytes: multipart upload, seeders) OR verifying they landed (the presigned
# flow, where the client PUTs directly and the API never sees the bytes). Both
# satisfy the invariant "this storage_key points at an object that exists"; only
# recording a key with neither is the bug.
BYTE_WRITE = re.compile(
    r"\bPutObject\s*\(|\bput_object\s*\(|\bupload_fileobj\s*\(|\bObjectExists\s*\(")

# Writing the column: an INSERT or UPDATE naming storage_key. Matched over the
# whole file because Go SQL is written as multi-line raw string literals.
SQL_WRITE = re.compile(
    r"(?is)\b(?:insert\s+into|update)\b[^;`\"']{0,400}?\bstorage_key\b")

SKIP_DIRS = {".git", "node_modules", "target", ".next", "__pycache__", "vendor"}
# The guard itself, and any sibling guard, necessarily contains the patterns it
# searches for. Excluding the directory rather than just this filename keeps a
# future check_*.py from tripping this one.
SKIP_RELDIRS = {"scripts"}
EXTS = (".go", ".rs", ".py", ".ts", ".tsx")


def scan():
    offenders, checked = [], 0
    for base, dirs, files in os.walk(ROOT):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for fn in files:
            if not fn.endswith(EXTS):
                continue
            path = os.path.join(base, fn)
            rel = os.path.relpath(path, ROOT)
            if rel.split(os.sep)[0] in SKIP_RELDIRS:
                continue
            try:
                src = open(path, encoding="utf-8").read()
            except (OSError, UnicodeDecodeError):
                continue
            if not SQL_WRITE.search(src):
                continue
            checked += 1
            if rel in ALLOWLIST:
                continue
            if not BYTE_WRITE.search(src):
                offenders.append(rel)
    return checked, offenders


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    checked, offenders = scan()

    if checked == 0:
        print("FAIL: found no file writing storage_key at all — the SQL pattern "
              "no longer matches this codebase and this guard is inert")
        return 1

    print(f"{checked} file(s) write a storage_key; "
          f"{len(ALLOWLIST)} allowlisted, {len(offenders)} unaccounted for")
    if args.verbose:
        for rel, why in sorted(ALLOWLIST.items()):
            print(f"  allowlisted: {rel}\n      reason: {why}")

    if offenders:
        print("\nORPHAN STORAGE KEY RISK — these write a storage_key to the "
              "database but never write the bytes:")
        for rel in offenders:
            print(f"  {rel}")
        print("\nEither write the bytes (storage.PutObject) before the row, or "
              "add the file to ALLOWLIST in this script with the reason it is "
              "safe. Do not remove this check.")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
