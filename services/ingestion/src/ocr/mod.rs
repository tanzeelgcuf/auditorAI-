pub mod doctr;
pub mod structured;

pub use doctr::DoctrBackend;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExtractedEntity {
    pub entity_type: String, // "invoice_line_item", "bank_transaction", "gl_entry"
    pub amount_cents: i64,
    pub currency: String,
    pub transaction_date: Option<chrono::NaiveDate>,
    pub counterparty: Option<String>,
    pub description: Option<String>,
    pub gl_account_code: Option<String>,
    pub transaction_ref: Option<String>, // source ref (e.g. GL "Num", OFX "FITID")
    pub page_number: i32,
    pub bbox: BoundingBox,
    pub confidence: f32,       // 0.0 - 1.0
    pub source_format: String, // "ocr" or "structured"
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BoundingBox {
    pub x: f32, // 0.0 - 1.0
    pub y: f32,
    pub width: f32,
    pub height: f32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProcessDocumentRequest {
    pub document_id: Uuid,
    pub storage_key: String,
    pub doc_type: String, // "invoice", "bank_statement", "gl_export"
    pub client_book_id: Uuid,
    pub column_map: std::collections::HashMap<String, String>, // per-book CSV mapping
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProcessDocumentResponse {
    pub entities: Vec<ExtractedEntity>,
}

#[derive(Debug, Error)]
pub enum OcrError {
    // NotFound and UnsupportedFormat were deleted 2026-09-17: never
    // constructed anywhere in the crate — the sidecar path reports
    // ProcessingFailed, and unknown extensions route to OCR by design.
    // Variants nothing can produce are dead enum arms.
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
    // The `name()` method was deleted 2026-09-17: zero callers crate-wide.
    // An identifier with no consumer is a label nobody reads.
    async fn process(
        &self,
        request: &ProcessDocumentRequest,
    ) -> Result<ProcessDocumentResponse, OcrError>;
}

// ── Format detection ──

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetectedFormat {
    Ocr,
    Csv,
    Xlsx,
    Ofx,
}

pub struct FormatDetector;

impl FormatDetector {
    pub fn from_extension(path: &str) -> DetectedFormat {
        let lower = path.to_lowercase();
        if lower.ends_with(".csv") {
            return DetectedFormat::Csv;
        }
        if lower.ends_with(".xlsx") || lower.ends_with(".xls") {
            return DetectedFormat::Xlsx;
        }
        if lower.ends_with(".ofx") || lower.ends_with(".qfx") {
            return DetectedFormat::Ofx;
        }
        DetectedFormat::Ocr
    }

    pub fn from_content(data: &[u8]) -> DetectedFormat {
        if data.starts_with(b"OFXHEADER") || data.starts_with(b"<?xml") {
            return DetectedFormat::Ofx;
        }
        let check_len = std::cmp::min(data.len(), 2048);
        if check_len > 0 && data[..check_len].contains(&b',') {
            return DetectedFormat::Csv;
        }
        DetectedFormat::Ocr
    }

    // detect() was deleted 2026-09-17: zero callers — process_document calls
    // from_extension/from_content separately precisely BECAUSE it must skip
    // content-sniffing for definitive OCR media types (is_image_or_pdf); this
    // convenience wrapper would have sniffed a stray comma in a PDF binary
    // into CSV had anything ever called it.
}
