# services/agent-runtime/graph/schema.py
# Pydantic models + LangGraph state (TypedDict).
# This service is LLM extraction/classification/linking ONLY — never arithmetic.

from pydantic import BaseModel, Field
from typing import Optional, List, TypedDict
from datetime import date
from uuid import UUID, uuid4


class EntityType(str):
    INVOICE = "invoice_line_item"
    BANK = "bank_transaction"
    GL = "gl_entry"


class EntitySubtype(str):
    STANDARD = "standard"
    CREDIT_NOTE = "credit_note"
    REFUND = "refund"
    VOID = "void"


class SourceFormat(str):
    OCR = "ocr"
    STRUCTURED = "structured"


class ExtractedEntity(BaseModel):
    id: UUID = Field(default_factory=uuid4)
    client_book_id: UUID
    source_document_id: UUID
    entity_type: str  # invoice_line_item, bank_transaction, gl_entry
    entity_subtype: str = EntitySubtype.STANDARD
    amount_cents: int
    currency: str = "USD"
    transaction_date: Optional[date] = None
    counterparty: Optional[str] = None
    description: Optional[str] = None
    gl_account_code: Optional[str] = None
    page_number: int = 1
    bbox: dict = Field(default_factory=dict)  # {x, y, width, height} normalized 0-1
    extraction_confidence: float = 1.0
    source_format: str = SourceFormat.OCR


class EntityLink(BaseModel):
    id: UUID = Field(default_factory=uuid4)
    client_book_id: UUID
    invoice_entity_id: Optional[UUID] = None
    bank_entity_id: Optional[UUID] = None
    gl_entity_id: Optional[UUID] = None
    link_confidence: float = 0.0
    status: str = "needs_review"  # auto_linked, needs_review, confirmed, rejected


class ReconciliationGroup(BaseModel):
    id: UUID = Field(default_factory=uuid4)
    client_book_id: UUID
    invoice_entity_ids: List[UUID] = Field(default_factory=list)
    bank_entity_ids: List[UUID] = Field(default_factory=list)
    gl_entity_ids: List[UUID] = Field(default_factory=list)
    link_confidence: float = 0.0
    status: str = "needs_review"  # auto_linked, needs_review, unmatched


class BookConfig(BaseModel):
    id: UUID
    tolerance_cents: int = 1
    auto_link_threshold: float = 0.85
    review_floor: float = 0.50
    tolerance_mode: str = "fixed"  # fixed, percentage, greater_of
    tolerance_percentage: Optional[float] = None


class ReconciliationResult(BaseModel):
    variance_cents: int
    exceeds_tolerance: bool
    calculation_formula: str
    rule_id: str
    rule_version: str
    severity: str  # info, low, medium, high


class GraphState(TypedDict, total=False):
    """LangGraph state flowing through extract → classify → link → verify."""
    client_book_id: UUID
    batch_id: UUID
    book_config: Optional[BookConfig]
    entries: List[dict]  # raw OCR/structured input
    entities: List[ExtractedEntity]  # extracted
    classified_entities: List[ExtractedEntity]  # after classify pass
    groups: List[ReconciliationGroup]
    unmatched: List[ExtractedEntity]
    results: List[ReconciliationResult]
    errors: List[str]
