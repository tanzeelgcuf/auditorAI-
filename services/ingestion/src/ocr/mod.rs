// services/ingestion/src/ocr/mod.rs
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExtractedEntity {
    pub entity_type: String,           // "invoice_line_item", "bank_transaction", "gl_entry"
    pub amount_cents: i64,
    pub currency: String,
    pub transaction_date: Option<chrono::NaiveDate>,
    pub counterparty: Option<String>,
    pub description: Option<String>,
    pub gl_account_code: Option<String>,
    pub page_number: i32,
    pub bbox: BoundingBox,
    pub confidence: f32,               // 0.0 - 1.0
    pub source_format: String,         // "ocr" or "structured"
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BoundingBox {
    pub x: f32,      // 0.0 - 1.0
    pub y: f32,
    pub width: f32,
    pub height: f32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProcessDocumentRequest {
    pub document_id: Uuid,
    pub storage_key: String,
    pub doc_type: String,  // "invoice", "bank_statement", "gl_export"
    pub client_book_id: Uuid,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProcessDocumentResponse {
    pub entities: Vec<ExtractedEntity>,
}

#[derive(Debug, Error)]
pub enum OcrError {
    #[error("document not found: {0}")]
    NotFound(String),
    #[error("unsupported format: {0}")]
    UnsupportedFormat(String),
    #[error("OCR processing failed: {0}")]
    ProcessingFailed(String),
    #[error("S3 error: {0}")]
    S3Error(String),
    #[error("sidecar communication error: {0}")]
    SidecarError(String),
    #[error("parsing error: {0}")]
    ParsingError(String),
}

#[async_trait]
pub trait OcrBackend: Send + Sync {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError>;
    fn name(&self) -> &'static str;
}