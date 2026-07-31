# services/agent-runtime/tests/test_extract.py
# Extraction + classification tests with a mocked Anthropic client (offline).

import sys
import os
import json
from uuid import UUID

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, GraphState
from graph.extract import extract_entities, classify_entities, _extract_json, _parse_amount_cents

BOOK_ID = UUID("11111111-1111-1111-1111-111111111111")
DOC_ID = UUID("22222222-2222-2222-2222-222222222222")


class _Messages:
    def __init__(self, owner):
        self.owner = owner

    def create(self, **kwargs):
        self.owner.last_prompt = kwargs.get("messages", [{}])[0].get("content", "")
        self.owner.last_system = kwargs.get("system", "")
        return {"content": [{"type": "text", "text": self.owner.response_text}]}


class FakeAnthropic:
    """Stand-in Anthropic client — returns canned JSON, records the prompt."""
    def __init__(self, response_text: str):
        self.response_text = response_text
        self.last_prompt = None
        self.last_system = None

    @property
    def messages(self):
        return _Messages(self)

    def create(self, **kwargs):  # pragma: no cover - legacy path
        return _Messages(self).create(**kwargs)


def make_state(entries):
    return {
        "client_book_id": BOOK_ID,
        "batch_id": UUID("33333333-3333-3333-3333-333333333333"),
        "entities": entries,
    }


# ---- _extract_json robustness ----

def test_extract_json_plain():
    assert _extract_json('[{"a": 1}]') == [{"a": 1}]


def test_extract_json_markdown_fence():
    text = '```json\n[{"a": 1}]\n```'
    assert _extract_json(text) == [{"a": 1}]


def test_extract_json_with_prose():
    text = 'Here are the items:\n[{"amount_cents": 100}]\n\nDone.'
    assert _extract_json(text) == [{"amount_cents": 100}]


def test_extract_json_garbage():
    assert _extract_json("not json at all") == []


# ---- _parse_amount_cents ----

def test_parse_amount_integer():
    assert _parse_amount_cents(100) == 100


def test_parse_amount_dollars_float():
    assert _parse_amount_cents(12.50) == 1250


def test_parse_amount_string_dollars():
    assert _parse_amount_cents("$12.50") == 1250


def test_parse_amount_string_cents():
    assert _parse_amount_cents("1250") == 1250


def test_parse_amount_negative():
    assert _parse_amount_cents("-500") == -500


def test_parse_amount_parenthesized():
    assert _parse_amount_cents("(500)") == -500


def test_parse_amount_garbage():
    assert _parse_amount_cents("N/A") == 0


# ---- extract_entities ----

def test_extract_entities_calls_llm_and_parses():
    canned = json.dumps([
        {
            "entity_type": "invoice_line_item",
            "entity_subtype": "standard",
            "amount_cents": 125000,
            "currency": "USD",
            "transaction_date": "2026-06-01",
            "counterparty": "Acme Corp",
            "description": "Web design services",
            "gl_account_code": None,
        },
        {
            "entity_type": "bank_transaction",
            "entity_subtype": "standard",
            "amount_cents": 125000,
            "currency": "USD",
            "transaction_date": "2026-06-05",
            "counterparty": "Acme Corp",
            "description": "ACH payment received",
            "gl_account_code": None,
        },
    ])

    client = FakeAnthropic(canned)
    state = make_state([
        {"text": "INV-123 $1250.00 Web design services", "description": "OCR line 1"},
        {"text": "ACH 1250.00 Acme Corp 06/05", "description": "OCR line 2"},
    ])

    result = extract_entities(state, client)

    assert "Extract raw values only" in client.last_prompt or "NEVER calculate" in client.last_prompt
    entities = result["classified_entities"]
    assert len(entities) == 2
    assert entities[0].entity_type == "invoice_line_item"
    assert entities[0].amount_cents == 125000
    assert entities[1].entity_type == "bank_transaction"


def test_extract_entities_error_appends_error():
    class BrokenClient:
        @property
        def messages(self):
            return self
        def create(self, **kwargs):
            raise RuntimeError("boom")

    state = make_state([{"text": "line 1"}])
    result = extract_entities(state, BrokenClient())
    assert "errors" in result
    assert len(result["errors"]) == 1
    assert "Extraction error" in result["errors"][0]


# ---- classify_entities ----

def test_classify_credit_note():
    e = ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type="invoice_line_item",
        description="CREDIT NOTE #5 for returned goods",
        amount_cents=5000,
    )
    state = make_state([])
    state["classified_entities"] = [e]
    result = classify_entities(state, None)  # no client needed for heuristics
    assert result["classified_entities"][0].entity_subtype == "credit_note"


def test_classify_refund():
    e = ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type="bank_transaction",
        description="REFUND from vendor",
        amount_cents=2500,
    )
    state = make_state([])
    state["classified_entities"] = [e]
    result = classify_entities(state, None)
    assert result["classified_entities"][0].entity_subtype == "refund"


def test_classify_void():
    e = ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type="gl_entry",
        description="VOIDED entry",
        amount_cents=1000,
    )
    state = make_state([])
    state["classified_entities"] = [e]
    result = classify_entities(state, None)
    assert result["classified_entities"][0].entity_subtype == "void"


def test_classify_leaves_standard_alone():
    e = ExtractedEntity(
        client_book_id=BOOK_ID,
        source_document_id=DOC_ID,
        entity_type="invoice_line_item",
        description="Office supplies",
        amount_cents=5000,
    )
    state = make_state([])
    state["classified_entities"] = [e]
    result = classify_entities(state, None)
    assert result["classified_entities"][0].entity_subtype == "standard"
