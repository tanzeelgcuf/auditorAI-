use super::{ExtractedEntity, BoundingBox, OcrBackend, OcrError, ProcessDocumentRequest, ProcessDocumentResponse};
use async_trait::async_trait;
use aws_sdk_s3::Client as S3Client;
use calamine::{open_workbook_from_rs, DataType, Reader, Xlsx};
use chrono::{Datelike, NaiveDate};
use csv::ReaderBuilder;
use regex::Regex;
use std::collections::HashMap;
use std::io::Cursor;
use std::sync::Arc;

// ── S3 download helper ──

pub(crate) async fn download_from_s3(
    client: &S3Client, bucket: &str, key: &str,
) -> Result<Vec<u8>, OcrError> {
    let resp = client
        .get_object()
        .bucket(bucket)
        .key(key)
        .send()
        .await
        .map_err(|e| OcrError::S3Error(format!("s3 fetch failed: {e}")))?;

    let bytes = resp
        .body
        .collect()
        .await
        .map_err(|e| OcrError::S3Error(format!("s3 body collect failed: {e}")))?;

    Ok(bytes.to_vec())
}

// ── Parse helpers ──

pub fn parse_amount(s: &str) -> Option<i64> {
    // Strip commas and currency symbols, keep digits . - +
    let cleaned: String = s
        .chars()
        .filter(|c| c.is_ascii_digit() || *c == '.' || *c == '-' || *c == '+')
        .collect();
    if cleaned.is_empty() || cleaned == "-" || cleaned == "+" || cleaned == "." {
        return None;
    }
    let v: f64 = cleaned.parse().ok()?;
    Some((v * 100.0).round() as i64)
}

// Strip commas from numeric strings before parsing amounts
fn strip_commas(s: &str) -> String {
    s.chars().filter(|&c| c != ',').collect()
}

/// Try common date formats, return first that parses.
pub fn parse_date(s: &str) -> Option<NaiveDate> {
    let s = s.trim().trim_matches('"').trim_matches('\'');
    if s.is_empty() {
        return None;
    }
    let fmts = &[
        "%Y-%m-%d",
        "%m/%d/%Y",
        "%d/%m/%Y",
        "%m/%d/%y",
        "%d-%m-%Y",
        "%d/%m/%y",
        "%Y%m%d",
        "%m-%d-%Y",
    ];
    for fmt in fmts {
        if let Ok(d) = NaiveDate::parse_from_str(s, fmt) {
            return Some(d);
        }
    }
    None
}

/// Map source columns to target fields using a column_map.
/// `column_map`: target_field -> source_column
/// Returns target_field -> value for matched columns.
pub fn map_columns(
    data: &HashMap<String, String>,
    column_map: &HashMap<String, String>,
) -> HashMap<String, String> {
    let mut out = HashMap::new();
    for (target, source) in column_map {
        if let Some(v) = data.get(source) {
            out.insert(target.clone(), v.clone());
        }
    }
    out
}

fn classify_entity_type(doc_type: &str) -> &'static str {
    match doc_type {
        "invoice" => "invoice_line_item",
        "bank_statement" => "bank_transaction",
        "gl_export" => "gl_entry",
        _ => "invoice_line_item",
    }
}

// ── CSV Parser ──

pub struct CsvParser {
    column_map: HashMap<String, String>,
    s3_client: Arc<S3Client>,
    bucket: String,
}

impl CsvParser {
    pub fn new(column_map: HashMap<String, String>, s3_client: Arc<S3Client>, bucket: String) -> Self {
        Self { column_map, s3_client, bucket }
    }
}

#[async_trait]
impl OcrBackend for CsvParser {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        let data = download_from_s3(&self.s3_client, &self.bucket, &request.storage_key).await?;
        let mut reader = ReaderBuilder::new()
            .flexible(true)
            .has_headers(true)
            .from_reader(data.as_slice());

        let headers = reader.headers()
            .map_err(|e| OcrError::ParsingError(format!("csv headers: {e}")))?
            .clone();

        let entity_type = classify_entity_type(&request.doc_type);
        let mut entities = Vec::new();

        for result in reader.records() {
            let record = result.map_err(|e| OcrError::ParsingError(format!("csv row: {e}")))?;
            let mut row_data = HashMap::new();
            for (i, h) in headers.iter().enumerate() {
                if let Some(v) = record.get(i) {
                    row_data.insert(h.to_string(), v.to_string());
                }
            }

            let mapped = map_columns(&row_data, &self.column_map);

            let raw_amount = mapped.get("amount").map(|s| s.as_str()).unwrap_or("0");
            let amount_cents = parse_amount(raw_amount).unwrap_or(0);
            let tx_date = mapped.get("date").and_then(|d| parse_date(d));
            let description = mapped.get("description").cloned();
            let counterparty = mapped.get("counterparty").cloned();
            let account_code = mapped.get("account_code").cloned();
            let currency = mapped.get("currency").cloned().unwrap_or_else(|| "USD".to_string());

            entities.push(ExtractedEntity {
                entity_type: entity_type.to_string(),
                amount_cents,
                currency,
                transaction_date: tx_date,
                counterparty,
                description,
                gl_account_code: account_code,
                page_number: 1,
                bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
                confidence: 1.0,
                source_format: "structured".to_string(),
            });
        }

        Ok(ProcessDocumentResponse { entities })
    }

    fn name(&self) -> &'static str {
        "csv"
    }
}

// ── XLSX Parser ──

pub struct XlsxParser {
    column_map: HashMap<String, String>,
    s3_client: Arc<S3Client>,
    bucket: String,
}

impl XlsxParser {
    pub fn new(column_map: HashMap<String, String>, s3_client: Arc<S3Client>, bucket: String) -> Self {
        Self { column_map, s3_client, bucket }
    }
}

#[async_trait]
impl OcrBackend for XlsxParser {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        let data = download_from_s3(&self.s3_client, &self.bucket, &request.storage_key).await?;
        let cursor = Cursor::new(data);
        let mut workbook: Xlsx<_> = open_workbook_from_rs(cursor)
            .map_err(|e| OcrError::ParsingError(format!("xlsx open: {e}")))?;

        let sheet_name = workbook
            .sheet_names()
            .first()
            .cloned()
            .unwrap_or_default();
        if sheet_name.is_empty() {
            return Ok(ProcessDocumentResponse { entities: vec![] });
        }

        let range = workbook
            .worksheet_range(&sheet_name)
            .ok_or_else(|| OcrError::ParsingError(format!("xlsx sheet '{sheet_name}' not found")))?
            .map_err(|e| OcrError::ParsingError(format!("xlsx sheet '{sheet_name}': {e}")))?;

        let mut rows = range.rows();
        let header_row = match rows.next() {
            Some(h) => h,
            None => return Ok(ProcessDocumentResponse { entities: vec![] }),
        };

        // Build header index: col index -> display string
        let headers: Vec<String> = header_row
            .iter()
            .map(|c| cell_string(c).to_lowercase())
            .collect();

        let entity_type = classify_entity_type(&request.doc_type);
        let mut entities = Vec::new();

        for row in rows {
            let mut row_data = HashMap::new();
            for (i, cell) in row.iter().enumerate() {
                if i < headers.len() {
                    row_data.insert(headers[i].clone(), cell_string(cell));
                }
            }

            let mapped = map_columns(&row_data, &self.column_map);

            let raw_amount = mapped.get("amount").map(|s| s.as_str()).unwrap_or("0");
            let amount_cents = parse_amount(raw_amount).unwrap_or(0);
            let tx_date = mapped.get("date").and_then(|d| parse_date(d));
            let description = mapped.get("description").cloned();
            let counterparty = mapped.get("counterparty").cloned();
            let account_code = mapped.get("account_code").cloned();
            let currency = mapped.get("currency").cloned().unwrap_or_else(|| "USD".to_string());

            entities.push(ExtractedEntity {
                entity_type: entity_type.to_string(),
                amount_cents,
                currency,
                transaction_date: tx_date,
                counterparty,
                description,
                gl_account_code: account_code,
                page_number: 1,
                bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
                confidence: 1.0,
                source_format: "structured".to_string(),
            });
        }

        Ok(ProcessDocumentResponse { entities })
    }

    fn name(&self) -> &'static str {
        "xlsx"
    }
}

fn cell_string(cell: &DataType) -> String {
    match cell {
        DataType::String(s) => s.clone(),
        DataType::Float(f) => {
            let s = format!("{f}");
            if s.ends_with(".0") { strip_commas(&s[..s.len()-2]) } else { strip_commas(&s) }
        }
        DataType::Int(i) => i.to_string(),
        DataType::DateTime(d) => {
            // Excel serial date: days since 1899-12-30
            let days = *d as i32;
            if let Some(epoch) = NaiveDate::from_ymd_opt(1899, 12, 30) {
                let target = epoch.num_days_from_ce().checked_add(days)
                    .unwrap_or(i32::MAX);
                if let Some(date) = NaiveDate::from_num_days_from_ce_opt(target) {
                    date.format("%Y-%m-%d").to_string()
                } else {
                    format!("{d}")
                }
            } else {
                format!("{d}")
            }
        }
        DataType::Bool(b) => b.to_string(),
        DataType::Error(e) => format!("error:{e:?}"),
        DataType::Empty => String::new(),
        DataType::Duration(d) => d.to_string(),
        DataType::DateTimeIso(s) => s.clone(),
        DataType::DurationIso(s) => s.clone(),
    }
}

// ── OFX Parser ──

pub struct OfxParser {
    s3_client: Arc<S3Client>,
    bucket: String,
}

impl OfxParser {
    pub fn new(s3_client: Arc<S3Client>, bucket: String) -> Self {
        Self { s3_client, bucket }
    }
}

#[async_trait]
impl OcrBackend for OfxParser {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        let data = download_from_s3(&self.s3_client, &self.bucket, &request.storage_key).await?;
        let content = String::from_utf8_lossy(&data);

        let stmt_trn_re = Regex::new(r"<STMTTRN>(.*?)</STMTTRN>")
            .map_err(|e| OcrError::ParsingError(format!("ofx regex: {e}")))?;

        let tag_re = Regex::new(r"<(\w+)>([^<]*)")
            .map_err(|e| OcrError::ParsingError(format!("ofx tag regex: {e}")))?;

        let mut entities = Vec::new();

        for cap in stmt_trn_re.captures_iter(&content) {
            let block = &cap[1];
            let mut fields: HashMap<String, String> = HashMap::new();

            for tag_cap in tag_re.captures_iter(block) {
                let name = tag_cap[1].trim().to_uppercase();
                let value = tag_cap[2].trim().to_string();
                if !value.is_empty() {
                    fields.insert(name, value);
                }
            }

            let raw_amount = fields.get("TRNAMT").map(|s| s.as_str()).unwrap_or("0");
            let amount_cents = parse_amount(raw_amount).unwrap_or(0);
            let raw_date = fields.get("DTPOSTED").or(fields.get("DTUSER"));
            let tx_date = raw_date.and_then(|d| parse_date(d));

            let fitid = fields.get("FITID").cloned();
            let name = fields.get("NAME").cloned();
            let memo = fields.get("MEMO").cloned();
            let description = match (&name, &memo) {
                (Some(n), Some(m)) => Some(format!("{n} — {m}")),
                (Some(n), None) => Some(n.clone()),
                (None, Some(m)) => Some(m.clone()),
                (None, None) => fitid.clone(),
            };

            entities.push(ExtractedEntity {
                entity_type: "bank_transaction".to_string(),
                amount_cents,
                currency: "USD".to_string(),
                transaction_date: tx_date,
                counterparty: name,
                description,
                gl_account_code: None,
                page_number: 1,
                bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
                confidence: 1.0,
                source_format: "structured".to_string(),
            });
        }

        Ok(ProcessDocumentResponse { entities })
    }

    fn name(&self) -> &'static str {
        "ofx"
    }
}

// ── Helper to create structured entities ──

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
        bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
        confidence: 1.0,
        source_format: "structured".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_amount_usd() {
        assert_eq!(parse_amount("150.00"), Some(15000));
        assert_eq!(parse_amount("2500.00"), Some(250000));
    }

    #[test]
    fn test_parse_amount_negative() {
        assert_eq!(parse_amount("-150.00"), Some(-15000));
    }

    #[test]
    fn test_parse_amount_no_decimal() {
        assert_eq!(parse_amount("100"), Some(10000));
    }

    #[test]
    fn test_parse_amount_with_currency() {
        assert_eq!(parse_amount("$1,500.00"), Some(150000));
        assert_eq!(parse_amount("€89.99"), Some(8999));
    }

    #[test]
    fn test_parse_date_formats() {
        assert_eq!(parse_date("2024-01-15"), Some(NaiveDate::from_ymd_opt(2024, 1, 15).unwrap()));
        assert_eq!(parse_date("01/15/2024"), Some(NaiveDate::from_ymd_opt(2024, 1, 15).unwrap()));
        assert_eq!(parse_date("20240115"), Some(NaiveDate::from_ymd_opt(2024, 1, 15).unwrap()));
    }

    #[test]
    fn test_map_columns() {
        let mut data = HashMap::new();
        data.insert("Date".to_string(), "2024-01-15".to_string());
        data.insert("Amount".to_string(), "150.00".to_string());

        let mut col_map = HashMap::new();
        col_map.insert("date".to_string(), "Date".to_string());
        col_map.insert("amount".to_string(), "Amount".to_string());

        let mapped = map_columns(&data, &col_map);
        assert_eq!(mapped.get("date").unwrap(), "2024-01-15");
        assert_eq!(mapped.get("amount").unwrap(), "150.00");
        assert!(mapped.get("description").is_none());
    }
}
