// services/ingestion/src/ocr/structured.rs
use super::{ExtractedEntity, OcrBackend, OcrError, ProcessDocumentRequest, ProcessDocumentResponse, BoundingBox};
use async_trait::async_trait;
use calamine::{open_workbook, Reader, Xlsx};
use csv::ReaderBuilder;
use chrono::NaiveDate;
use std::collections::HashMap;
use uuid::Uuid;

pub fn detect_format(storage_key: &str, _content: &[u8]) -> Result<String, OcrError> {
    let lower = storage_key.to_lowercase();
    if lower.ends_with(".csv") { return Ok("csv".to_string()); }
    if lower.ends_with(".xlsx") || lower.ends_with(".xls") { return Ok("xlsx".to_string()); }
    if lower.ends_with(".ofx") || lower.ends_with(".qfx") { return Ok("ofx".to_string()); }
    Ok("ocr".to_string())
}

pub struct CsvParser {
    column_map: HashMap<String, String>, // target_field -> source_column
}

impl CsvParser {
    pub fn new(column_map: HashMap<String, String>) -> Self {
        Self { column_map }
    }

    fn get_col(&self, target: &str) -> Option<&String> {
        self.column_map.get(target)
    }
}

#[async_trait]
impl OcrBackend for CsvParser {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        // In real impl: download from S3, then parse
        // For now, return empty - this is a scaffold
        Ok(ProcessDocumentResponse { entities: vec![] })
    }

    fn name(&self) -> &'static str {
        "csv"
    }
}

pub struct XlsxParser {
    column_map: HashMap<String, String>,
}

impl XlsxParser {
    pub fn new(column_map: HashMap<String, String>) -> Self {
        Self { column_map }
    }

    fn get_col(&self, target: &str) -> Option<&String> {
        self.column_map.get(target)
    }
}

#[async_trait]
impl OcrBackend for XlsxParser {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        Ok(ProcessDocumentResponse { entities: vec![] })
    }

    fn name(&self) -> &'static str {
        "xlsx"
    }
}

pub struct OfxParser;

#[async_trait]
impl OcrBackend for OfxParser {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        // Parse OFX/QFX using ofx crate or Python ofxparse sidecar
        Ok(ProcessDocumentResponse { entities: vec![] })
    }

    fn name(&self) -> &'static str {
        "ofx"
    }
}

// Helper to create entities from structured data rows
pub fn create_structured_entity(
    entity_type: &str,
    amount_cents: i64,
    currency: &str,
    date: Option<NaiveDate>,
    counterparty: Option<String>,
    description: Option<String>,
    gl_account_code: Option<String>,
    page_number: i32,
) -> ExtractedEntity {
    ExtractedEntity {
        entity_type: entity_type.to_string(),
        amount_cents,
        currency: currency.to_string(),
        transaction_date: date,
        counterparty,
        description,
        gl_account_code,
        page_number,
        bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 }, // N/A for structured
        confidence: 1.0,
        source_format: "structured".to_string(),
    }
}