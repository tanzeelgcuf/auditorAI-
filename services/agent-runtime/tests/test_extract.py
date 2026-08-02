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


# ---- fixture eval set (doc 13 / Round 7) ----
# Lock the 4 real invoice totals from invoices_batch_june2026.pdf. The extraction
# contract outputs amount_raw (verbatim page string); the deterministic parser
# converts. Asserting these exact cents catches a regression in either the
# contract or the parser before it silently drifts — the permanent eval guard.
def test_invoice_fixture_raw_strings_convert_exactly():
    fixtures = {
        "342.50": 34250,   # INV-1001
        "128.75": 12875,   # INV-1002
        "899.00": 89900,   # BCH-2291
        "215.00": 21500,   # MP-5502
        "$342.50": 34250,  # with symbol
        "1,500.00": 150000,  # comma-thousands
    }
    for raw, want in fixtures.items():
        assert _parse_amount_cents(raw) == want, f"{raw!r} -> {_parse_amount_cents(raw)}, want {want}"


def test_parse_raw_string_negative():
    assert _parse_amount_cents("-45.00") == -4500
    assert _parse_amount_cents("-342.50") == -34250


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


# ---- citation grounding (Round 7 / task #6) ----
# A cited source index must resolve to a raw OCR entity carrying the total
# amount. A correct amount with a wrong citation is worse than a wrong amount —
# it looks trustworthy until a user clicks it (the whole traceability feature).
def _cited_total_is_grounded(cited_texts, want_cents):
    for src in cited_texts:
        digits = "".join(c for c in src if c.isdigit())
        if digits and abs(int(digits) - want_cents) < 5:
            return True
    return False


def test_citation_grounded_when_total_cited():
    # Page 3 eval: cites address + $899.00 + $899.00. Grounded because the
    # $899.00 lines carry the total (89900) even though the address is noise.
    assert _cited_total_is_grounded(
        ["900 Cirrus Park Drive, Austin, TX 78701", "$899.00", "$899.00"], 89900)


def test_citation_ungrounded_when_no_amount_cited():
    # Page 2 failure mode: model cited only "Net 15" — no dollar amount. The
    # citation gives the user nothing to verify; must flag as ungrounded.
    assert not _cited_total_is_grounded(["Net 15"], 12875)


def test_citation_ungrounded_when_wrong_amount_cited():
    # A cite pointing at $150.00 line when the total is $899.00 is ungrounded —
    # the user would highlight the wrong line.
    assert not _cited_total_is_grounded(["$150.00", "$749.00"], 89900)


# ---- citation re-grounding (Round 7 / task #6) ----
# The model's source_indices may cite boilerplate ("Net 15") that carries no
# dollar value even when the total is correct. Deterministic re-grounding points
# the citation at the raw entity whose text actually contains the total.

def test_ground_citations_keeps_valid_cited():
    from run_extraction import _ground_citations
    texts = ["$342.50", "$342.50", "$250.00", "Net 15"]
    # Cited [0, 1] already carry the total -> preserved.
    assert _ground_citations([0, 1], texts, 34250) == [0, 1]

def test_ground_citations_repairs_boilerplate_cite():
    from run_extraction import _ground_citations
    texts = ["$128.75", "$128.75", "$128.75", "$5.15", "Net 15"]
    # Cited [4] = "Net 15" has no dollar value -> re-point onto the $128.75 lines.
    got = _ground_citations([4], texts, 12875)
    assert 4 not in got
    assert texts[got[0]] == "$128.75"

def test_ground_citations_fallback_to_cited_when_no_total_on_page():
    from run_extraction import _ground_citations
    texts = ["$150.00", "$749.00"]
    # No entity carries 89900 -> keep the model's citation (nothing better).
    assert _ground_citations([0], texts, 89900) == [0]
