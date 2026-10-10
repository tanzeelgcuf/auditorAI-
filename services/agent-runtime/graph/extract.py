# services/agent-runtime/graph/extract.py
# Entity classification — LLM CLASSIFICATION ONLY. NO MONEY, NO MATH.
#
# WHAT CHANGED AND WHY (read before editing):
#
# This node used to ask the model for `amount_raw` and convert it to cents in
# Python (`_parse_amount_cents`, deleted). Three things were wrong with that:
#
#   1. The money was ALREADY EXACT before this node ran. In production the rows
#      in state["entities"] come from mcp.get_pending_entities, i.e. straight
#      out of extracted_entities.amount_cents, which services/ingestion (Rust,
#      rust_decimal) parsed. Round-tripping that integer through an LLM and a
#      Python float could only degrade it.
#   2. The conversion used binary floating point (`int(round(float(s) * 100))`)
#      and treated a bare integer as cents, while the Rust parser treats a bare
#      integer as whole dollars. "1250" therefore meant $12.50 here and
#      $1,250.00 there — a 100x disagreement between the two parsers, with a
#      passing test suite on both sides.
#   3. Unparseable input returned 0, so a garbled amount became a $0.00 entity
#      that reconciles with zero variance instead of failing the document.
#
# So this node no longer sees money at all. It returns classification only, and
# every entity is rebuilt from its SOURCE ROW: id, amount_cents, currency,
# source_document_id, page_number and bbox are copied, never regenerated. The
# model contributes entity_type/entity_subtype and nothing that can move a
# number. Per the architecture rule, money conversion lives in Rust
# (services/ingestion for parsing, services/verification for arithmetic).
#
# The join key is `source_index`, the [n] label the model is shown. Without it
# the output had no reference back to the input row, which is how the previous
# version silently lost the database id: it constructed ExtractedEntity without
# `id=`, so pydantic minted a fresh uuid4 (schema.py:30). Those invented ids do
# not exist in extracted_entities, and reconciliation_group_members.
# extracted_entity_id is a FOREIGN KEY — so every group built from this node's
# output was rejected by create_entity_link with 404 "entity not found"
# (mcp.go:202-207), swallowed by persist_groups' except (mcp_client:103), and
# reported as "groups persisted written=0".

import json
import re
import structlog
from typing import Any, Dict, List, Optional
from datetime import date, datetime
from uuid import UUID

from .schema import ExtractedEntity, GraphState

logger = structlog.get_logger()

EXTRACTION_SYSTEM = (
    "You are a precise financial document classification system. "
    "Classify exactly what you see — no inference, no calculation. "
    "NEVER perform arithmetic. NEVER compute, restate, or re-type any monetary "
    "amount: the amounts are already parsed and are not yours to touch. "
    "You return only a type, a subtype, and the index of the row you classified."
)

EXTRACTION_PROMPT_TEMPLATE = """Classify each numbered row below.

For each row you classify, return a JSON object with fields:
- source_index: the integer in the [n] label of the row you are classifying.
  REQUIRED. One object per row, and never invent an index you were not shown.
- entity_type: choose by the DOCUMENT CONTEXT below, not by a guess:
  - "invoice_line_item" when this page/block is an INVOICE (it has an invoice
    number like INV-xxxx, line items, subtotal/total)
  - "bank_transaction" when it is a BANK statement/ledger entry
  - "gl_entry" ONLY when it is explicitly a general-ledger entry
  An invoice's line items and totals are ALWAYS invoice_line_item — never
  gl_entry or bank_transaction just because they contain a dollar amount.
- entity_subtype: "standard" | "credit_note" | "refund" | "void"

CRITICAL RULES:
- Do NOT return amounts. Not as a number, not as a string, not in a
  description. The monetary value of every row is already known exactly and
  will be taken from the source record, not from your reply. Any amount you
  emit is discarded.
- NEVER calculate. No totals, no sums, no variances, no unit conversions.
  You must NOT convert a dollar amount to cents — that is arithmetic, it is
  done elsewhere, and it is not part of your output.
- If you see a total, do not sum the line items to check it.
- Do not reconcile or judge — classification only.

Return ONLY a JSON array of objects, no prose.

EXAMPLES of what each type looks like in real exports:
- invoice_line_item: rows from an INVOICE (client billing): descriptions
  like "Consulting services Jan"; counterparties are customers.
- bank_transaction: rows from a BANK export (OFX STMTTRN blocks, "ACH
  DEBIT" memos): the bank's own record of money moving. A bank's record
  of its own transactions is bank_transaction even when the rows name
  customers.
- gl_entry: rows from a GENERAL LEDGER export, INCLUDING double-entry
  exports with Debit AND Credit columns (one row per side, account codes
  like "4000-Consulting Income").

Classify by what the ROW is, not by which document type you guess the
file is.

OCR/Data:
{context}
"""

CLASSIFICATION_SYSTEM = (
    "You are a financial document classifier. Confirm entity types and detect "
    "subtypes (credit notes, refunds, voids). Classification only — no arithmetic."
)


class _FakeMessagesResponse:
    """Minimal stand-in for Anthropic's response when client is a fake/mock."""
    def __init__(self, text: str):
        self.text = text
        self.content = [{"type": "text", "text": text}]


def _extract_json(text: str) -> Any:
    """Robustly extract JSON from an LLM response (handles code fences)."""
    text = text.strip()
    # Strip markdown fences
    fence = re.search(r"```(?:json)?\s*(.*?)```", text, re.DOTALL)
    if fence:
        text = fence.group(1).strip()
    # Try direct parse
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass
    # Try first array bracket
    start = text.find("[")
    end = text.rfind("]")
    if start != -1 and end != -1 and end > start:
        try:
            return json.loads(text[start:end + 1])
        except json.JSONDecodeError:
            pass
    return []


def _parse_date(value: Any) -> Optional[date]:
    if not value:
        return None
    if isinstance(value, date):
        return value
    for fmt in ("%Y-%m-%d", "%m/%d/%Y", "%d/%m/%Y"):
        try:
            return datetime.strptime(str(value), fmt).date()
        except ValueError:
            continue
    return None


def _row_cents(row: Dict[str, Any]) -> Optional[int]:
    """Read the already-parsed integer cents off a source row.

    This is a READ, not a conversion, and that is the whole point: the value was
    produced by services/ingestion (Rust, rust_decimal) and is authoritative.
    Anything that is not already an exact integer is refused — a float, a
    string, or a missing field means the row did not come from the ingestion
    parser, and guessing what it meant is exactly the bug this replaced.
    Callers must fail the row, never substitute 0.
    """
    if "amount_cents" not in row:
        return None
    value = row["amount_cents"]
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    # A JSON number that round-trips exactly (e.g. 34250.0 from a lax encoder)
    # is accepted; 342.50 is NOT — that is dollars, and converting it here is
    # the arithmetic this module must not perform.
    if isinstance(value, float) and value.is_integer():
        return int(value)
    return None



def _row_str(row: Dict[str, Any], key: str) -> Optional[str]:
    value = row.get(key)
    if value is None:
        return None
    text = str(value).strip()
    return text or None


def extract_entities(state: GraphState, client: Any) -> GraphState:
    """LangGraph node: classify the batch's source rows via the LLM.

    Money is copied from the source row, never from the model's reply. See the
    module header for why. A row without exact integer `amount_cents` or without
    an `id` is REFUSED and recorded in state["errors"] — it means the row did not
    come from the ingestion parser, and a fabricated amount reconciles silently.
    """
    logger.info("classifying source rows", batch_id=str(state.get("batch_id")))

    entries = state.get("entities") or []
    if not entries:
        logger.warning("no rows to classify")
        return state

    rows: Dict[int, Dict[str, Any]] = {}
    errors: List[str] = []
    context_lines: List[str] = []

    for i, e in enumerate(entries, 1):
        if not isinstance(e, dict):
            errors.append(f"row {i}: expected a source record, got {type(e).__name__}")
            continue
        if _row_cents(e) is None:
            errors.append(
                f"row {i}: no exact integer amount_cents on the source record "
                f"(got {e.get('amount_cents')!r}); refusing to infer an amount"
            )
            continue
        if not e.get("id"):
            errors.append(f"row {i}: source record has no id; a group built on it "
                          f"would fail the extracted_entity_id foreign key")
            continue
        rows[i] = e

    if errors:
        logger.error("rejected source rows", count=len(errors))
        state["errors"] = state.get("errors", []) + errors
    if not rows:
        state["classified_entities"] = []
        return state

    # OFX/QFX entities are bank_transaction BY CONSTRUCTION — the STMTTRN
    # format is a bank statement by definition, so the parser's typing is
    # content-derived and authoritative. The LLM must not re-type it:
    # observed live 2026-09-19, the model re-typed correctly-parsed bank
    # rows as invoices and the 3-way trio never assembled. A content-derived
    # type is not a guess for the model to confirm.
    bank_rows: Dict[int, Dict[str, Any]] = {
        i: e for i, e in rows.items()
        if e.get("entity_type") == "bank_transaction"
        and (e.get("source_format") or "") == "structured"
    }
    # gl_account_code is the same class of content-derived signal: an account
    # code exists ONLY in a general-ledger export, so a structured entity
    # carrying one is a gl_entry by construction. Observed 2026-09-19: the GL
    # fixture's rows carry the SAME descriptions as the invoice fixture (the
    # GL is the ledger OF those invoices), so no content-only classifier can
    # tell them apart — the model typed them all as invoices and the GL leg
    # never existed. The account code is the evidence the model does not have.
    gl_rows: Dict[int, Dict[str, Any]] = {
        i: e for i, e in rows.items()
        if i not in bank_rows
        and _row_str(e, "gl_account_code")
    }
    llm_rows: Dict[int, Dict[str, Any]] = {
        i: e for i, e in rows.items()
        if i not in bank_rows and i not in gl_rows
    }

    # The model sees text only. Amounts are deliberately NOT rendered into
    # the context: it has no reason to read them and no way to return them.
    context_lines = []
    for i, e in llm_rows.items():
        text = _row_str(e, "description") or _row_str(e, "text") or ""
        context_lines.append(f"[{i}] {text}")

    context = "\n".join(context_lines)

    try:
        # No model kwarg here: the adapter (or a real Anthropic client) owns
        # the model name — a hardcoded literal here was ignored by the
        # adapter and would have silently pinned a wrong model for a real
        # Anthropic client.
        response = client.messages.create(
            max_tokens=4000,
            temperature=0,
            system=EXTRACTION_SYSTEM,
            messages=[{"role": "user", "content": EXTRACTION_PROMPT_TEMPLATE.format(context=context)}],
        )
        raw = _get_response_text(response)
        parsed = _extract_json(raw)

        entities: List[ExtractedEntity] = []
        seen: set = set()
        # Content-derived entities (bank from OFX, GL from the account code)
        # pass through with the parser's type — no model round trip, nothing
        # for the model to get wrong.
        for i, e in list(bank_rows.items()) + list(gl_rows.items()):
            fallback = "bank_transaction" if i in bank_rows else "gl_entry"
            entities.append(ExtractedEntity(
                id=UUID(str(e["id"])),
                client_book_id=UUID(str(e.get("client_book_id") or state.get("client_book_id"))),
                source_document_id=UUID(str(e["source_document_id"])),
                entity_type=e.get("entity_type") or fallback,
                entity_subtype=e.get("entity_subtype") or "standard",
                amount_cents=_row_cents(e),
                currency=e.get("currency") or "USD",
                transaction_date=_parse_date(e.get("transaction_date")),
                counterparty=_row_str(e, "counterparty"),
                description=_row_str(e, "description"),
                gl_account_code=_row_str(e, "gl_account_code"),
                page_number=int(e.get("page_number") or 1),
                bbox=e.get("bbox") or {},
                extraction_confidence=float(e.get("extraction_confidence") or 1.0),
                source_format=e.get("source_format") or "structured",
            ))
        for item in parsed:
            if not isinstance(item, dict):
                continue
            idx = item.get("source_index")
            try:
                idx = int(idx)
            except (TypeError, ValueError):
                state["errors"] = state.get("errors", []) + [
                    f"classification dropped: source_index missing/not an int ({item.get('source_index')!r})"
                ]
                continue
            # Scoped to llm_rows, not rows: the model was only SHOWN llm_rows'
            # indexes, so an index pointing at a content-derived bank row is a
            # hallucination and must be rejected, never resolved and re-typed.
            row = llm_rows.get(idx)
            if row is None:
                state["errors"] = state.get("errors", []) + [
                    f"classification dropped: source_index {idx} was never shown to the model"
                ]
                continue
            if idx in seen:
                continue  # first classification of a row wins; duplicates ignored
            seen.add(idx)

            # Identity, money and provenance come from the ROW. Only the type and
            # subtype come from the model.
            entities.append(ExtractedEntity(
                id=UUID(str(row["id"])),
                client_book_id=UUID(str(row.get("client_book_id") or state.get("client_book_id"))),
                source_document_id=UUID(str(row["source_document_id"])),
                # A row the model did not type KEEPS the parser's type — the
                # parser's classification is evidence; defaulting to
                # invoice_line_item FABRICATES a type for rows the model went
                # silent on (observed 2026-09-19: the GL rows' unclassified
                # batch would have come back as ten invoices).
                entity_type=item.get("entity_type") or row.get("entity_type") or "invoice_line_item",
                # The model may emit entity_subtype: null — coerce to the default
                # so the pydantic schema (which rejects None for str) accepts it.
                entity_subtype=item.get("entity_subtype") or "standard",
                amount_cents=_row_cents(row),
                currency=row.get("currency") or "USD",
                transaction_date=_parse_date(row.get("transaction_date")),
                counterparty=_row_str(row, "counterparty"),
                description=_row_str(row, "description"),
                gl_account_code=_row_str(row, "gl_account_code"),
                page_number=int(row.get("page_number") or 1),
                bbox=row.get("bbox") or {},
                extraction_confidence=float(row.get("extraction_confidence") or 1.0),
                source_format=row.get("source_format") or "ocr",
            ))

        unclassified = sorted(set(llm_rows) - seen)
        if unclassified:
            state["errors"] = state.get("errors", []) + [
                f"rows {unclassified} were shown to the model but never classified"
            ]

        state["classified_entities"] = entities
        logger.info("classification complete",
                    entity_count=len(entities), unclassified=len(unclassified))
        return state
    except Exception as e:
        logger.error("classification failed", error=str(e))
        state["errors"] = state.get("errors", []) + [f"Extraction error: {e}"]
        return state


def classify_entities(state: GraphState, client: Any) -> GraphState:
    """LangGraph node: confirm entity types and detect subtypes (credit/refund/void)."""
    logger.info("classifying entities", batch_id=str(state.get("batch_id")))

    entities = state.get("classified_entities") or []
    if not entities:
        logger.warning("no classified entities to refine")
        return state

    # Heuristic subtype detection (works offline, no LLM needed for obvious cases)
    for e in entities:
        desc = (e.description or "").upper()
        if e.entity_subtype != "standard":
            continue
        if any(k in desc for k in ("CREDIT MEMO", "CREDIT NOTE", "CREDITS")):
            e.entity_subtype = "credit_note"
        elif any(k in desc for k in ("REFUND", "REIMBURSEMENT")):
            e.entity_subtype = "refund"
        elif any(k in desc for k in ("VOID", "VOIDED", "REVERSAL", "REVERSED")):
            e.entity_subtype = "void"

    state["classified_entities"] = entities
    logger.info("classification complete", entity_count=len(entities))
    return state


def _get_response_text(response: Any) -> str:
    """Extract text from an Anthropic response object (works with fakes/dicts too)."""
    if isinstance(response, _FakeMessagesResponse):
        return response.text
    if isinstance(response, dict):
        content = response.get("content")
        if isinstance(content, list):
            parts = []
            for block in content:
                if isinstance(block, dict) and block.get("type") == "text":
                    parts.append(block.get("text", ""))
            if parts:
                return "\n".join(parts)
        return response.get("text", str(response))
    if hasattr(response, "content"):
        parts = []
        for block in response.content:
            if getattr(block, "type", None) == "text":
                parts.append(block.text)
            elif isinstance(block, dict) and block.get("type") == "text":
                parts.append(block.get("text", ""))
        if parts:
            return "\n".join(parts)
    return str(response)
