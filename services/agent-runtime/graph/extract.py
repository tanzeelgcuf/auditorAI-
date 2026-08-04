# services/agent-runtime/graph/extract.py
# Entity extraction + classification — LLM EXTRACTION ONLY. NO MATH.

import json
import re
import structlog
from typing import Any, List, Optional
from datetime import date, datetime
from uuid import UUID

from .schema import ExtractedEntity, GraphState

logger = structlog.get_logger()

EXTRACTION_SYSTEM = (
    "You are a precise financial document data extraction system. "
    "Extract exactly what you see — no inference, no calculation. "
    "NEVER perform arithmetic. NEVER compute totals. "
    "If a line shows a total, extract it as-is; do not verify it against line items."
)

EXTRACTION_PROMPT_TEMPLATE = """Extract structured entities from the OCR/structured data below.

For each item, return a JSON object with fields:
- entity_type: choose by the DOCUMENT CONTEXT below, not by a guess:
  - "invoice_line_item" when this page/block is an INVOICE (it has an invoice
    number like INV-xxxx, line items, subtotal/total)
  - "bank_transaction" when it is a BANK statement/ledger entry
  - "gl_entry" ONLY when it is explicitly a general-ledger entry
  An invoice's line items and totals are ALWAYS invoice_line_item — never
  gl_entry or bank_transaction just because they contain a dollar amount.
- entity_subtype: "standard" | "credit_note" | "refund" | "void"
- amount_raw: the amount EXACTLY as it appears in the OCR/Data context below,
  as a string. MUST be a value you actually see THERE — hundreds/thousands may
  vary. The examples above ("1,500.00", "-45.00") are SHAPE illustrations, not
  values that appear in the input; never copy them.
  Include the sign for credits/refunds.
  NEVER multiply, scale, convert, or compute — copy the digits and symbols verbatim.
- currency: ISO code
- transaction_date: "YYYY-MM-DD" or null
- counterparty: vendor/customer name or null
- description: short description or null
- gl_account_code: string or null

CRITICAL RULES:
- Extract raw values ONLY. NEVER calculate totals, sums, variances, OR unit
  conversions. You must NOT convert a dollar amount to cents — that is arithmetic
  and is done elsewhere, not by you.
- Return amount_raw as the literal text/symbols you see in the OCR/Data context.
  If a dollar symbol is present, include it; otherwise return the digits as-is.
  Never output a dollar string that is not in the context.
- If you see a total, return it as-is without summing line items to verify.
- Preserve sign: credit notes / refunds are negative in amount_raw (e.g. "-45.00").
- Do not reconcile or judge — extraction only.

Return ONLY a JSON array of objects, no prose.

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


def _parse_amount_cents(value: Any) -> int:
    """Convert whatever the model returned to integer cents. No arithmetic — just parse."""
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(round(value * 100))
    s = str(value).strip()
    neg = s.startswith("-") or (s.startswith("(") and s.endswith(")"))
    s = re.sub(r"[^0-9.]", "", s)
    try:
        parsed = int(round(float(s) * 100)) if "." in s else int(s or 0)
    except ValueError:
        return 0
    return -parsed if neg else parsed


def extract_entities(state: GraphState, client: Any) -> GraphState:
    """LangGraph node: extract structured entities from raw OCR input via Claude."""
    logger.info("extracting entities", batch_id=str(state.get("batch_id")))

    entries = state.get("entities") or []
    if not entries:
        logger.warning("no entities to extract")
        return state

    context = "\n".join(
        f"[{i}] {e.get('description') or e.get('text') or str(e)}"
        for i, e in enumerate(entries, 1)
    )

    try:
        response = client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=4000,
            temperature=0,
            system=EXTRACTION_SYSTEM,
            messages=[{"role": "user", "content": EXTRACTION_PROMPT_TEMPLATE.format(context=context)}],
        )
        raw = _get_response_text(response)
        parsed = _extract_json(raw)

        entities: List[ExtractedEntity] = []
        for item in parsed:
            if not isinstance(item, dict):
                continue
            # amount_raw is the verbatim page string (e.g. "$342.50"); the
            # deterministic _parse_amount_cents does the only money conversion —
            # never ask the model to compute cents (doc 13: unit conversion is
            # arithmetic the LLM must not touch). amount_cents kept as a fallback
            # for older prompt versions.
            amount_source = item.get("amount_raw") or item.get("amount_cents")
            entities.append(ExtractedEntity(
                client_book_id=state.get("client_book_id"),
                source_document_id=UUID(item.get("source_document_id", str(state.get("batch_id")))),
                entity_type=item.get("entity_type", "invoice_line_item"),
                # The model may emit entity_subtype: null — coerce to the default
                # so the pydantic schema (which rejects None for str) accepts it.
                entity_subtype=item.get("entity_subtype") or "standard",
                amount_cents=_parse_amount_cents(amount_source),
                currency=item.get("currency", "USD"),
                transaction_date=_parse_date(item.get("transaction_date")),
                counterparty=item.get("counterparty"),
                description=item.get("description"),
                gl_account_code=item.get("gl_account_code"),
                page_number=int(item.get("page_number", 1)),
                bbox=item.get("bbox") or {},
                extraction_confidence=float(item.get("extraction_confidence", 1.0)),
                source_format=item.get("source_format", "ocr"),
            ))

        state["classified_entities"] = entities
        logger.info("extraction complete", entity_count=len(entities))
        return state
    except Exception as e:
        logger.error("extraction failed", error=str(e))
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
