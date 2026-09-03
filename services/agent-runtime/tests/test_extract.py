# services/agent-runtime/tests/test_extract.py
# Classification tests with a mocked Anthropic client (offline).

import sys
import os
import json
from uuid import UUID, uuid4

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from graph.schema import ExtractedEntity, GraphState
from graph.extract import extract_entities, classify_entities, _extract_json, _row_cents

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


def source_row(**overrides):
    """A row shaped like mcp.HandleGetPendingEntities returns (mcp.go:102-118).

    The production input to this node is a DATABASE ROW whose amount_cents was
    already parsed by services/ingestion — not a bare OCR string. The old
    fixtures passed {"text": ...} only, which no production caller ever sends.
    """
    row = {
        "id": str(uuid4()),
        "client_book_id": str(BOOK_ID),
        "source_document_id": str(DOC_ID),
        "entity_type": "invoice_line_item",
        "entity_subtype": "",
        "amount_cents": 34250,
        "currency": "USD",
        "transaction_date": "2026-06-01",
        "counterparty": "Acme Corp",
        "description": "Web design services",
        "gl_account_code": "",
        "page_number": 1,
        "bbox": {"x": 0.1, "y": 0.2, "width": 0.3, "height": 0.04},
        "extraction_confidence": 0.98,
        "source_format": "structured",
    }
    row.update(overrides)
    return row


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


# ---- _row_cents: a READ of an exact integer, never a conversion ----
# _parse_amount_cents (deleted) used to live here. Its tests asserted the bug:
# `_parse_amount_cents("1250") == 1250` treated a bare integer as CENTS while
# services/ingestion's parse_amount treats a bare integer as whole DOLLARS
# (structured.rs AMOUNT_CASES: "100" -> 10000). The same string meant $12.50 in
# Python and $1,250.00 in Rust, and both suites were green.

def test_row_cents_reads_integer():
    assert _row_cents({"amount_cents": 34250}) == 34250
    assert _row_cents({"amount_cents": -4500}) == -4500
    assert _row_cents({"amount_cents": 0}) == 0


def test_row_cents_accepts_exact_float_integer():
    # A lax JSON encoder may render 34250 as 34250.0 — exact, so admissible.
    assert _row_cents({"amount_cents": 34250.0}) == 34250


def test_row_cents_refuses_dollars_as_float():
    # 342.50 is DOLLARS. Converting it here is the arithmetic this module must
    # not perform, so the row is refused rather than multiplied by 100.
    assert _row_cents({"amount_cents": 342.50}) is None


def test_row_cents_refuses_strings_and_missing():
    for value in ("34250", "$342.50", "342.50", "N/A", "", None):
        assert _row_cents({"amount_cents": value}) is None, value
    assert _row_cents({}) is None


def test_row_cents_refuses_bool():
    # bool is an int subclass in Python; True must not become 1 cent.
    assert _row_cents({"amount_cents": True}) is None


# ---- extract_entities ----

def test_extract_entities_calls_llm_and_classifies():
    rows = [
        source_row(amount_cents=125000, description="Web design services"),
        source_row(amount_cents=125000, description="ACH payment received",
                   counterparty="Acme Corp", transaction_date="2026-06-05"),
    ]
    canned = json.dumps([
        {"source_index": 1, "entity_type": "invoice_line_item", "entity_subtype": "standard"},
        {"source_index": 2, "entity_type": "bank_transaction", "entity_subtype": "standard"},
    ])

    client = FakeAnthropic(canned)
    result = extract_entities(make_state(rows), client)

    assert "NEVER calculate" in client.last_prompt
    entities = result["classified_entities"]
    assert len(entities) == 2
    assert entities[0].entity_type == "invoice_line_item"
    assert entities[1].entity_type == "bank_transaction"
    assert entities[0].amount_cents == 125000
    assert result.get("errors", []) == []


def test_amounts_come_from_the_row_not_the_model():
    """The FK + money regression, asserted directly.

    The model is given an amount-free context and its reply is not trusted for
    money or identity. Even when it emits a contradictory amount and a different
    source_document_id, the entity must carry the ROW's id and cents — otherwise
    reconciliation_group_members.extracted_entity_id (a FOREIGN KEY to
    extracted_entities) cannot resolve, and create_entity_link 404s.
    """
    row = source_row(amount_cents=89900, description="BCH-2291 balance due")
    canned = json.dumps([{
        "source_index": 1,
        "entity_type": "invoice_line_item",
        "entity_subtype": "standard",
        # everything below is an attempt to move the number or the provenance:
        "amount_cents": 1,
        "amount_raw": "$1.00",
        "id": str(uuid4()),
        "source_document_id": str(uuid4()),
        "currency": "EUR",
    }])

    result = extract_entities(make_state([row]), FakeAnthropic(canned))
    e = result["classified_entities"][0]

    assert e.amount_cents == 89900
    assert str(e.id) == row["id"]
    assert str(e.source_document_id) == str(DOC_ID)
    assert e.currency == "USD"


def test_prompt_context_carries_no_amounts():
    row = source_row(amount_cents=89900, description="BCH-2291 balance due")
    client = FakeAnthropic(json.dumps([{"source_index": 1, "entity_type": "invoice_line_item"}]))
    extract_entities(make_state([row]), client)
    assert "89900" not in client.last_prompt
    assert "899.00" not in client.last_prompt
    assert "BCH-2291 balance due" in client.last_prompt


def test_row_without_exact_cents_is_refused_not_zeroed():
    # The old code returned 0 here, producing a $0.00 entity that reconciles
    # with zero variance. It must fail the row instead.
    rows = [source_row(amount_cents="342.50"), source_row(amount_cents=12875)]
    # Indices refer to the ORIGINAL enumeration, so the surviving row stays [2];
    # refusing a row must not shift the labels of the rows after it.
    client = FakeAnthropic(json.dumps([
        {"source_index": 2, "entity_type": "invoice_line_item"},
    ]))
    result = extract_entities(make_state(rows), client)

    assert "[2] " in client.last_prompt and "[1] " not in client.last_prompt
    assert len(result["classified_entities"]) == 1
    assert result["classified_entities"][0].amount_cents == 12875
    assert any("no exact integer amount_cents" in err for err in result["errors"])


def test_row_without_id_is_refused():
    result = extract_entities(
        make_state([source_row(id="")]),
        FakeAnthropic(json.dumps([{"source_index": 1, "entity_type": "invoice_line_item"}])),
    )
    assert result["classified_entities"] == []
    assert any("no id" in err for err in result["errors"])


def test_unknown_source_index_is_dropped_with_an_error():
    result = extract_entities(
        make_state([source_row()]),
        FakeAnthropic(json.dumps([{"source_index": 7, "entity_type": "invoice_line_item"}])),
    )
    assert result["classified_entities"] == []
    assert any("never shown to the model" in err for err in result["errors"])


def test_missing_source_index_is_dropped_with_an_error():
    result = extract_entities(
        make_state([source_row()]),
        FakeAnthropic(json.dumps([{"entity_type": "invoice_line_item"}])),
    )
    assert result["classified_entities"] == []
    assert any("source_index missing" in err for err in result["errors"])


def test_unclassified_row_is_reported():
    result = extract_entities(
        make_state([source_row(), source_row()]),
        FakeAnthropic(json.dumps([{"source_index": 1, "entity_type": "invoice_line_item"}])),
    )
    assert len(result["classified_entities"]) == 1
    assert any("never classified" in err for err in result["errors"])


def test_extract_entities_error_appends_error():
    class BrokenClient:
        @property
        def messages(self):
            return self
        def create(self, **kwargs):
            raise RuntimeError("boom")

    state = make_state([source_row()])
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


# ---- entity_type context anchoring (Round 7 / gl_entry fix) ----
# A 5/5-consistent gl_entry misclassification on an invoice page was traced to
# prompt framing: the open enum "invoice_line_item | bank_transaction | gl_entry"
# let a small model drift to the catch-all. The prompt must anchor to document
# context. Assert the production prompt carries the anchoring rule.

def test_production_prompt_anchors_invoice_entity_type():
    from graph.extract import EXTRACTION_PROMPT_TEMPLATE
    assert "DOCUMENT CONTEXT" in EXTRACTION_PROMPT_TEMPLATE
    assert "never gl_entry" in EXTRACTION_PROMPT_TEMPLATE.lower() or "ALWAYS invoice_line_item" in EXTRACTION_PROMPT_TEMPLATE
    # Invoice-first ordering (no structural bias toward gl_entry)
    et = EXTRACTION_PROMPT_TEMPLATE.split("entity_type:")[1]
    assert et.find("invoice_line_item") < et.find("gl_entry")

def test_production_prompt_defaults_to_invoice():
    from graph.extract import extract_entities, EXTRACTION_PROMPT_TEMPLATE
    # No gl_entry default — the fallback is invoice_line_item
    assert "item.get(\"entity_type\", \"invoice_line_item\")" in open(
        os.path.join(os.path.dirname(__file__), "..", "graph", "extract.py")).read()
