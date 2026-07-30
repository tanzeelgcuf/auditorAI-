# services/agent-runtime/graph/extract.py
# Entity extraction from OCR output — LLM EXTRACTION ONLY. NO MATH.

import structlog
from typing import Any
from anthropic import Anthropic
from langfuse.decorators import observe

from .schema import ExtractedEntity, GraphState

logger = structlog.get_logger()


class ExtractionError(Exception):
    pass


@observe(name="extract_entities")
async def extract_entities(state: GraphState, client: Anthropic) -> GraphState:
    """Extract structured entities from raw OCR text using Claude.

    This node reads raw OCR output and returns structured entities.
    It does NOT perform any arithmetic or compliance evaluation.
    """
    logger.info("extracting entities", batch_id=str(state.batch_id))

    if not state.entities:
        logger.warning("no entities to extract", batch_id=str(state.batch_id))
        return state

    # Build prompt from OCR entities
    ocr_text = _build_ocr_context(state.entities)

    prompt = f"""You are an entity extraction system for financial document reconciliation.
Extract structured entities from the OCR text below.

For each extractable item, return:
- entity_type: one of "invoice_line_item", "bank_transaction", "gl_entry"
- entity_subtype: "standard", "credit_note", "refund", or "void"
- amount_cents: the amount in integer cents (never calculate totals — extract the raw amount shown)
- currency: the currency code (USD, EUR, etc.)
- transaction_date: the date in YYYY-MM-DD format
- counterparty: the vendor/customer name
- description: any item description
- gl_account_code: if present

CRITICAL RULES:
- Extract raw values only — NEVER calculate totals or perform arithmetic
- If you see a total, extract it as-is — don't sum line items to verify it
- If you see a negative amount (credit/refund), extract it as-is (negative cents)
- Do not attempt to reconcile or verify — extraction only

OCR Context:
{ocr_text}
"""

    try:
        response = client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=4000,
            temperature=0,
            system="You are a precise document data extraction system. Extract exactly what you see — no inference, no calculation.",
            messages=[{"role": "user", "content": prompt}],
        )

        # Parse response into structured entities
        # In production, use structured output format
        extracted = _parse_extraction_response(response)

        logger.info("extraction complete",
                     batch_id=str(state.batch_id),
                     entity_count=len(extracted))

        # Replace raw OCR entities with extracted ones
        # In production: merge extraction results with original bbox data
        state.classified_entities = extracted
        return state

    except Exception as e:
        logger.error("extraction failed", batch_id=str(state.batch_id), error=str(e))
        state.errors.append(f"Extraction error: {e}")
        return state


@observe(name="classify_entities")
async def classify_entities(state: GraphState, client: Anthropic) -> GraphState:
    """Refine entity classifications — confirms entity_type and detects subtypes."""
    logger.info("classifying entities", batch_id=str(state.batch_id))

    if not state.classified_entities:
        logger.warning("no classified entities to refine")
        return state

    # Quick second-pass classification for ambiguous entities
    prompt = _build_classification_prompt(state.classified_entities)

    try:
        response = client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=2000,
            temperature=0,
            system="You are a financial document classifier. Confirm entity types and detect subtypes (credit notes, refunds, voids).",
            messages=[{"role": "user", "content": prompt}],
        )

        logger.info("classification complete", batch_id=str(state.batch_id))
        return state

    except Exception as e:
        logger.error("classification failed", batch_id=str(state.batch_id), error=str(e))
        state.errors.append(f"Classification error: {e}")
        return state


def _build_ocr_context(entities: list[ExtractedEntity]) -> str:
    """Build a human-readable context string from OCR entities."""
    lines = []
    for i, e in enumerate(entities, 1):
        lines.append(
            f"[{i}] Type: {e.entity_type} | Amount: {e.amount_cents / 100:.2f} {e.currency} | "
            f"Date: {e.transaction_date} | Counterparty: {e.counterparty or 'N/A'} | "
            f"Desc: {e.description or 'N/A'} | Page: {e.page_number} | "
            f"Confidence: {e.extraction_confidence:.2f} | Source: {e.source_format}"
        )
    return "\n".join(lines)


def _build_classification_prompt(entities: list[ExtractedEntity]) -> str:
    """Build a prompt for entity type refinement."""
    lines = []
    for i, e in enumerate(entities, 1):
        lines.append(f"[{i}] {e.description or 'N/A'} — detected as {e.entity_type}")
    return "Confirm or correct the entity types:\n" + "\n".join(lines)


def _parse_extraction_response(response: Any) -> list[ExtractedEntity]:
    """Parse Claude's response into structured entities.

    In production: use Anthropic's structured output format with a schema.
    For now: placeholder that returns the input as-is.
    """
    # Placeholder — real impl parses JSON/content blocks from response
    return []