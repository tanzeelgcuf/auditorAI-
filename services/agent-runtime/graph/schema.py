# services/agent-runtime/graph/schema.py
from dataclasses import dataclass, field
from typing import Optional
from datetime import date
from uuid import UUID


@dataclass
class ExtractedEntity:
    id: UUID
    client_book_id: UUID
    source_document_id: UUID
    entity_type: str  # invoice_line_item, bank_transaction, gl_entry
    entity_subtype: str  # standard, credit_note, refund, void
    amount_cents: int
    currency: str
    transaction_date: Optional[date]
    counterparty: Optional[str]
    description: Optional[str]
    gl_account_code: Optional[str]
    page_number: int
    bbox: dict  # {x, y, width, height} normalized 0-1
    extraction_confidence: float
    source_format: str  # ocr, structured


@dataclass
class EntityLink:
    id: UUID
    client_book_id: UUID
    invoice_entity_id: Optional[UUID]
    bank_entity_id: Optional[UUID]
    gl_entity_id: Optional[UUID]
    link_confidence: float
    status: str  # auto_linked, needs_review, confirmed, rejected


@dataclass
class ReconciliationGroup:
    id: UUID
    client_book_id: UUID
    link_confidence: float
    status: str


@dataclass
class BookConfig:
    id: UUID
    tolerance_cents: int
    auto_link_threshold: float = 0.85
    review_floor: float = 0.50
    tolerance_mode: str = "fixed"  # fixed, percentage, greater_of
    tolerance_percentage: Optional[float] = None


@dataclass
class ReconciliationResult:
    variance_cents: int
    exceeds_tolerance: bool
    calculation_formula: str
    rule_id: str
    rule_version: str
    severity: str  # info, low, medium, high


@dataclass
class GraphState:
    """State that flows through the LangGraph nodes."""
    client_book_id: UUID
    batch_id: UUID
    entities: list[ExtractedEntity] = field(default_factory=list)
    classified_entities: list[ExtractedEntity] = field(default_factory=list)
    reconciliation_groups: list[ReconciliationGroup] = field(default_factory=list)
    results: list[ReconciliationResult] = field(default_factory=list)
    errors: list[str] = field(default_factory=list)