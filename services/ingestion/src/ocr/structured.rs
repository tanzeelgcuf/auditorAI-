use super::{ExtractedEntity, BoundingBox, OcrBackend, OcrError, ProcessDocumentRequest, ProcessDocumentResponse};
use async_trait::async_trait;
use aws_sdk_s3::Client as S3Client;
use aws_sdk_s3::config::{Builder as S3ConfigBuilder, Region};
use calamine::{open_workbook_from_rs, DataType, Reader, Xlsx};
use chrono::{Datelike, NaiveDate};
use csv::ReaderBuilder;
use regex::Regex;
use rust_decimal::prelude::ToPrimitive;
use rust_decimal::Decimal;
use std::collections::HashMap;
use std::io::Cursor;
use std::sync::Arc;

// ── S3 client with MinIO path-style support ──

/// Build an S3 client that works against both MinIO (path-style, custom endpoint)
/// and AWS. Reads AWS_ENDPOINT_URL / AWS_REGION / credentials from env.
pub(crate) async fn build_s3_client() -> S3Client {
    let config = aws_config::load_from_env().await;
    let mut b = S3ConfigBuilder::from(&config);
    b.set_force_path_style(Some(true));
    b.set_region(Some(Region::new("us-east-1")));
    if let Ok(ep) = std::env::var("AWS_ENDPOINT_URL") {
        if !ep.is_empty() {
            b.set_endpoint_url(Some(ep));
        }
    }
    S3Client::from_conf(b.build())
}

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

// ── Money parsing ──
//
// This is the ONLY place a source document's amount string becomes the integer
// cents written to extracted_entities.amount_cents, which services/verification
// then reconciles and reports on. The previous implementation was three lines and
// had three silent defects:
//
//  1. `let v: f64 = cleaned.parse().ok()?; Some((v * 100.0).round() as i64)` put
//     money through binary floating point — in a service whose sibling
//     services/verification/src/decimal_math/mod.rs:3 states "NEVER use f32/f64
//     for money. ONLY rust_decimal::Decimal." rust_decimal was already a declared
//     dependency of THIS crate (Cargo.toml:20) and went entirely unused.
//  2. The character filter kept only digits and `.`/`-`/`+` and discarded every
//     other byte, so "(45.00)" — the accounting negative that QuickBooks, Xero and
//     most GL exports emit for credits — parsed as +4500. A credit became a debit
//     with no error. "1.500,00" (European) lost its comma and parsed as 150 cents
//     instead of 150000, understating by 1000x. "45.00-" (trailing sign, common in
//     SAP/mainframe exports) failed to parse entirely.
//  3. All three call sites used `.unwrap_or(0)`, so anything unparseable became
//     $0.00 and reconciled as a real number.
//
// Ambiguity is now REJECTED, never guessed: "1.500" is $1.50 under one convention
// and $1,500 under another, and a single field carries no evidence for either, so
// it returns None and the caller fails the document with the row number. A loud
// failure an operator can fix beats a silent 1000x error in an audit report.
const CURRENCY_SYMBOLS: &[char] = &[
    '$', '€', '£', '¥', '₹', '¢', '₩', '₽', '₺', '₴', '₦', '₱', '₡', '₪', '¤', '﷼',
];

/// Detach the sign, returning (is_negative, unsigned_body).
/// Handles accounting parentheses and both leading and trailing minus.
fn split_sign(s: &str) -> (bool, String) {
    let t = s.trim();
    if let Some(inner) = t.strip_prefix('(').and_then(|x| x.strip_suffix(')')) {
        return (true, inner.trim().to_string());
    }
    if let Some(rest) = t.strip_prefix('-') {
        return (true, rest.trim().to_string());
    }
    if let Some(rest) = t.strip_suffix('-') {
        return (true, rest.trim().to_string());
    }
    (false, t.strip_prefix('+').unwrap_or(t).trim().to_string())
}

/// Keep digits and the two separator characters; silently drop currency symbols,
/// whitespace (including the NBSP and the Swiss apostrophe used as thousands
/// separators); reject anything else.
///
/// Letters are rejected on purpose. "45.00 CR" and "45.00 DR" carry the sign in a
/// suffix, and quietly ignoring it would reintroduce exactly the credit-becomes-
/// debit inversion this function exists to prevent.
fn keep_numeric(body: &str) -> Option<String> {
    let mut kept = String::with_capacity(body.len());
    for c in body.chars() {
        if c.is_ascii_digit() || c == '.' || c == ',' {
            kept.push(c);
        } else if c.is_whitespace() || c == '\u{00a0}' || c == '\'' || CURRENCY_SYMBOLS.contains(&c) {
            continue;
        } else {
            return None;
        }
    }
    if kept.is_empty() { None } else { Some(kept) }
}

/// Every thousands group must be exactly 3 digits, and the leading group 1-3.
fn valid_grouping(int_part: &str, sep: char) -> bool {
    let groups: Vec<&str> = int_part.split(sep).collect();
    if groups.len() < 2 {
        return !int_part.is_empty() && int_part.chars().all(|c| c.is_ascii_digit());
    }
    if groups[0].is_empty() || groups[0].len() > 3 {
        return false;
    }
    groups.iter().enumerate().all(|(i, g)| {
        g.chars().all(|c| c.is_ascii_digit()) && (i == 0 || g.len() == 3)
    })
}

/// Normalize an unsigned amount to canonical `digits[.digits]`, or None when the
/// separator convention cannot be determined. Pure string work — no arithmetic.
fn normalize_decimal(body: &str) -> Option<String> {
    let kept = keep_numeric(body)?;
    let dots = kept.matches('.').count();
    let commas = kept.matches(',').count();

    // Which character is the decimal point, if any?
    let dec: Option<char> = if dots > 0 && commas > 0 {
        // Both present: the rightmost is the decimal point ("1.234,56" / "1,234.56").
        if kept.rfind('.') > kept.rfind(',') { Some('.') } else { Some(',') }
    } else if dots + commas == 0 {
        None // bare integer dollars
    } else {
        let (sep, n) = if dots > 0 { ('.', dots) } else { (',', commas) };
        if n > 1 {
            None // repeated single separator can only be thousands grouping
        } else {
            let tail = kept.rsplit(sep).next().unwrap_or("");
            match tail.len() {
                // 1 or 2 trailing digits: a decimal point. Nobody groups thousands
                // into 1 or 2 digits.
                1 | 2 => Some(sep),
                // Exactly 3: indistinguishable from a thousands separator.
                // Treated as grouping ONLY for the separator that cannot be a
                // decimal mark in the same string — which, with one separator and
                // no other evidence, is neither. Reject.
                3 => return None,
                _ => return None,
            }
        }
    };

    let (int_raw, frac) = match dec {
        Some(d) => {
            let idx = kept.rfind(d)?;
            (&kept[..idx], &kept[idx + 1..])
        }
        None => (kept.as_str(), ""),
    };

    // Whatever is not the decimal separator must be valid thousands grouping.
    // An EMPTY integer part is legitimate when a decimal separator is present
    // (".99" = 99 cents, a form some exports do emit), so grouping is only checked
    // when there are integer digits to check — validating "" as a group rejected
    // ".99" outright until a test caught it.
    let thou = match dec {
        Some('.') => ',',
        Some(',') => '.',
        _ => if dots > 0 { '.' } else { ',' },
    };
    if !int_raw.is_empty() && !valid_grouping(int_raw, thou) {
        return None;
    }
    let int_part: String = int_raw.chars().filter(|c| c.is_ascii_digit()).collect();
    if int_part.is_empty() && frac.is_empty() {
        return None;
    }
    if !frac.chars().all(|c| c.is_ascii_digit()) {
        return None;
    }
    // More than 2 decimal places cannot be represented in cents, and choosing a
    // rounding for the operator is a financial calculation this service must not make.
    if frac.len() > 2 {
        return None;
    }
    let int_part = if int_part.is_empty() { "0".to_string() } else { int_part };
    Some(if frac.is_empty() {
        int_part
    } else {
        format!("{int_part}.{:0<2}", frac) // pad "5" -> "50"
    })
}

/// Parse a source-document amount string to exact integer cents.
/// Returns None for anything unparseable or ambiguous — callers must NOT default
/// it to zero.
pub fn parse_amount(s: &str) -> Option<i64> {
    let (neg, body) = split_sign(s);
    let canonical = normalize_decimal(&body)?;
    // rust_decimal, not f64: exact base-10, so scale-2 values survive the x100.
    let d = Decimal::from_str_exact(&canonical).ok()?;
    if d.scale() > 2 {
        return None;
    }
    // Exact: d has at most 2 decimal places, so d*100 has a zero fractional part
    // and trunc() discards nothing.
    let cents = (d * Decimal::from(100)).trunc().to_i64()?;
    Some(if neg { -cents } else { cents })
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
    // OFX timestamps are YYYYMMDDHHMMSS (14 chars) — take the date portion.
    let s = if s.len() >= 8 && s.chars().all(|c| c.is_ascii_digit()) && s.len() >= 14 {
        &s[..8]
    } else {
        s
    };
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
/// Resolve the amount for a row: use the column-map amount if present, else fall
/// back to whichever of Debit/Credit is non-empty (double-entry GL exports).
pub fn resolve_amount(mapped: &HashMap<String, String>, raw: &HashMap<String, String>) -> String {
    if let Some(amt) = mapped.get("amount") {
        if !amt.trim().is_empty() {
            return amt.clone();
        }
    }
    // Double-entry exports name the sides differently. The mapped amount points
    // at one side (e.g. debit_amount); a credit row has that side empty, so fall
    // back to the OTHER side. Handles plain "Debit"/"Credit" and the
    // "*_amount" variants (Prompt B: stress GL uses debit_amount/credit_amount).
    for key in ["Debit", "Credit", "debit", "credit", "debit_amount", "credit_amount"] {
        if let Some(v) = raw.get(key) {
            if !v.trim().is_empty() {
                return v.clone();
            }
        }
    }
    String::new()
}

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

        for (row_idx, result) in reader.records().enumerate() {
            let record = result.map_err(|e| OcrError::ParsingError(format!("csv row: {e}")))?;
            let mut row_data = HashMap::new();
            for (i, h) in headers.iter().enumerate() {
                if let Some(v) = record.get(i) {
                    row_data.insert(h.to_string(), v.to_string());
                }
            }

            let mapped = map_columns(&row_data, &self.column_map);

            // Double-entry CSV exports carry Debit + Credit columns; the column map
            // points amount at one of them. If the mapped amount is empty but the
            // OTHER side exists in the raw row, fall back to it (doc 08 §1).
            let raw_amount = resolve_amount(&mapped, &row_data);
            // Fail the document, do NOT default to 0. This loop already aborts on a
            // malformed row, so an amount the parser cannot read unambiguously is
            // reported the same way — with the row number, so the operator can see
            // which cell to fix. `unwrap_or(0)` silently reconciled the book against
            // an amount that was never in it.
            let amount_cents = parse_amount(&raw_amount).ok_or_else(|| {
                OcrError::ParsingError(format!(
                    "row {}: cannot parse amount {raw_amount:?} unambiguously; \
                     expected forms like 1234.56, 1,234.56, (45.00) or -45.00",
                    row_idx + 2 // +1 for 0-index, +1 for the header row
                ))
            })?;
            let tx_date = mapped.get("date").and_then(|d| parse_date(d));
            let description = mapped.get("description").cloned();
            let counterparty = mapped.get("counterparty").cloned();
            let account_code = mapped.get("account_code").cloned();
            let currency = mapped.get("currency").cloned().unwrap_or_else(|| "USD".to_string());
            // The transaction reference (e.g. QuickBooks "Num" column) identifies a
            // journal entry so debit/credit legs can be keyed on it, not a heuristic.
            let tx_ref = mapped.get("transaction_ref")
                .or_else(|| row_data.get("Num"))
                .or_else(|| row_data.get("Ref"))
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty());

            entities.push(ExtractedEntity {
                entity_type: entity_type.to_string(),
                amount_cents,
                currency,
                transaction_date: tx_date,
                counterparty,
                description,
                gl_account_code: account_code,
                transaction_ref: tx_ref,
                page_number: 1,
                bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
                confidence: 1.0,
                source_format: "structured".to_string(),
            });
        }

        // Double-entry GL exports emit one row per side (debit AND credit for the
        // same journal entry). Collapse pairs sharing (date, counterparty, abs
        // amount) into one entity so the GL sums once, not twice.
        entities = dedupe_gl_pairs(entities);

        Ok(ProcessDocumentResponse { entities })
    }

    fn name(&self) -> &'static str {
        "csv"
    }
}

/// Collapse debit/credit pairs in a GL export to one entity per journal entry.
/// Two rows with the same date + counterparty + |amount| are one transaction.
pub fn dedupe_gl_pairs(entities: Vec<ExtractedEntity>) -> Vec<ExtractedEntity> {
    use std::collections::HashSet;
    let mut seen: HashSet<String> = HashSet::new();
    let mut out = Vec::new();
    for e in entities {
        // Prefer the transaction reference (e.g. GL "Num") as the dedup key —
        // it uniquely identifies a journal entry. Fall back to
        // (date, counterparty, abs amount) only when no ref exists (OCR entities,
        // OFX without FITID) — that heuristic can wrongly collapse two distinct
        // entries that happen to share all three.
        let key = match &e.transaction_ref {
            Some(r) if !r.is_empty() => format!("ref:{}", r),
            _ => format!(
                "heur:{:?}|{}|{}",
                e.transaction_date,
                e.counterparty.clone().unwrap_or_default(),
                e.amount_cents.abs()
            ),
        };
        if seen.insert(key) {
            out.push(e);
        }
    }
    out
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

        // enumerate() after rows.next() consumed the header, so row_idx 0 is the
        // second spreadsheet row — the same offset the CSV path uses.
        for (row_idx, row) in rows.enumerate() {
            let mut row_data = HashMap::new();
            for (i, cell) in row.iter().enumerate() {
                if i < headers.len() {
                    row_data.insert(headers[i].clone(), cell_string(cell));
                }
            }

            let mapped = map_columns(&row_data, &self.column_map);

            // Double-entry CSV exports carry Debit + Credit columns; the column map
            // points amount at one of them. If the mapped amount is empty but the
            // OTHER side exists in the raw row, fall back to it (doc 08 §1).
            let raw_amount = resolve_amount(&mapped, &row_data);
            // Fail the document, do NOT default to 0. This loop already aborts on a
            // malformed row, so an amount the parser cannot read unambiguously is
            // reported the same way — with the row number, so the operator can see
            // which cell to fix. `unwrap_or(0)` silently reconciled the book against
            // an amount that was never in it.
            let amount_cents = parse_amount(&raw_amount).ok_or_else(|| {
                OcrError::ParsingError(format!(
                    "row {}: cannot parse amount {raw_amount:?} unambiguously; \
                     expected forms like 1234.56, 1,234.56, (45.00) or -45.00",
                    row_idx + 2 // +1 for 0-index, +1 for the header row
                ))
            })?;
            let tx_date = mapped.get("date").and_then(|d| parse_date(d));
            let description = mapped.get("description").cloned();
            let counterparty = mapped.get("counterparty").cloned();
            let account_code = mapped.get("account_code").cloned();
            let currency = mapped.get("currency").cloned().unwrap_or_else(|| "USD".to_string());
            // The transaction reference (e.g. QuickBooks "Num" column) identifies a
            // journal entry so debit/credit legs can be keyed on it, not a heuristic.
            let tx_ref = mapped.get("transaction_ref")
                .or_else(|| row_data.get("Num"))
                .or_else(|| row_data.get("Ref"))
                .map(|s| s.trim().to_string())
                .filter(|s| !s.is_empty());

            entities.push(ExtractedEntity {
                entity_type: entity_type.to_string(),
                amount_cents,
                currency,
                transaction_date: tx_date,
                counterparty,
                description,
                gl_account_code: account_code,
                transaction_ref: tx_ref,
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

        // (?s) = dotall: STMTTRN blocks span multiple lines; `.` must match \n.
        let stmt_trn_re = Regex::new(r"(?s)<STMTTRN>(.*?)</STMTTRN>")
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

            // TRNAMT is mandatory in OFX 1.x/2.x <STMTTRN>. Defaulting a missing or
            // unreadable amount to "0" produced a $0.00 bank transaction that
            // reconciliation then treated as real; the FITID identifies the offending
            // transaction to the operator.
            let raw_amount = fields.get("TRNAMT").map(|s| s.as_str()).ok_or_else(|| {
                OcrError::ParsingError(format!(
                    "STMTTRN {}: missing <TRNAMT>",
                    fields.get("FITID").map(|s| s.as_str()).unwrap_or("<no FITID>")
                ))
            })?;
            let amount_cents = parse_amount(raw_amount).ok_or_else(|| {
                OcrError::ParsingError(format!(
                    "STMTTRN {}: cannot parse <TRNAMT> {raw_amount:?} unambiguously",
                    fields.get("FITID").map(|s| s.as_str()).unwrap_or("<no FITID>")
                ))
            })?;
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
                transaction_ref: fitid.clone(),
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
        transaction_ref: None,
        page_number,
        bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
        confidence: 1.0,
        source_format: "structured".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // The four original tests covered "150.00", "-150.00", "100", "$1,500.00" and
    // "€89.99" — all happy path, all single-convention. Every defect fixed in
    // parse_amount survived precisely because nothing here exercised an accounting
    // negative, a European separator or a trailing sign. Their assertions are kept
    // verbatim below, in the same table as the regressions.
    const AMOUNT_CASES: &[(&str, Option<i64>, &str)] = &[
        // original assertions, unchanged
        ("150.00", Some(15000), "usd"),
        ("2500.00", Some(250000), "usd"),
        ("-150.00", Some(-15000), "leading minus"),
        ("100", Some(10000), "bare integer is whole dollars"),
        ("$1,500.00", Some(150000), "symbol + thousands"),
        ("€89.99", Some(8999), "non-ascii symbol"),
        ("471.25", Some(47125), "map_columns fixture below depends on this"),
        // regressions: each of these was silently WRONG before
        ("(45.00)", Some(-4500), "was +4500 — accounting credit read as a debit"),
        ("(1,234.56)", Some(-123456), "parens with thousands"),
        ("1.500,00", Some(150000), "was 150 — European, understated 1000x"),
        ("45.00-", Some(-4500), "was None then 0 — trailing sign, SAP/mainframe"),
        ("1 234,56", Some(123456), "space thousands, comma decimal"),
        ("1'234.56", Some(123456), "Swiss apostrophe thousands"),
        ("1.234.567,89", Some(123456789), "European multi-group"),
        ("1,234,567.89", Some(123456789), "US multi-group"),
        (".99", Some(99), "no integer part"),
        ("45.5", Some(4550), "one decimal digit pads to 50"),
        ("0.01", Some(1), "one cent"),
        ("0.00", Some(0), "explicit zero is legitimate"),
        ("8.65", Some(865), "typical two-dp value, exact here by construction"),
        // f64 evidence, measured not assumed: 1.15_f64 * 100.0 is
        // 114.99999999999999, and 1.005_f64 * 100.0 is 100.49999999999999.
        // The old code's .round() hid the first (114.99… -> 115) and silently
        // decided the second (100.49… -> 100, when the written value is nearer
        // 101). Rounding money is itself a calculation, so the exact path takes
        // 1.15 and REJECTS 1.005 rather than choosing for the firm.
        ("1.15", Some(115), "1.15_f64 * 100.0 = 114.99999999999999"),
        ("999999999999.99", Some(99999999999999), "large, still in i64"),
        // The four pilot invoice totals (services/ingestion/test_fixtures, used
        // by the agent-runtime eval). They are pinned HERE because this is where
        // the conversion happens: deleting agent-runtime's
        // graph/extract.py::_parse_amount_cents removed the only test that had
        // ever asserted them, and a value nothing asserts is a value that drifts.
        ("$342.50", Some(34250), "pilot INV-1001 total"),
        ("$128.75", Some(12875), "pilot INV-1002 total"),
        ("$899.00", Some(89900), "pilot BCH-2291 total"),
        ("$215.00", Some(21500), "pilot MP-5502 total"),
        ("97401", Some(9740100), "bare integer in a CSV amount column is dollars"),
        // ambiguity and garbage: None, so the caller fails the document
        ("1.500", None, "$1.50 or EUR 1,500 — unknowable from one field"),
        ("1,500", None, "$1,500 or EUR 1,50 — unknowable from one field"),
        ("1234.567", None, "3dp is not cents; picking a rounding is calculation"),
        ("1.005", None, "the classic f64 rounding trap, rejected outright"),
        ("45.00 CR", None, "letters may carry the sign; never ignore them"),
        ("45.00%", None, "a percentage is not an amount — reject, don't strip"),
        ("4/5", None, "a fraction or a date fragment, not an amount"),
        ("12,34,567.89", None, "Indian lakh grouping unsupported — reject"),
        ("", None, "empty"),
        ("-", None, "sign only"),
        (".", None, "separator only"),
        ("$", None, "symbol only"),
        ("abc", None, "not a number"),
        ("1-2", None, "not a number"),
    ];

    #[test]
    fn test_parse_amount_table() {
        for (input, expect, why) in AMOUNT_CASES {
            assert_eq!(
                parse_amount(input), *expect,
                "parse_amount({input:?}) — {why}"
            );
        }
    }

    /// Pins exactness across every cent in 0.00–9.99 so a future edit cannot
    /// reintroduce a float hop unnoticed.
    ///
    /// Scope, stated honestly: this is a GUARD, not a reproduction of the old
    /// bug. The previous implementation ended in `(v * 100.0).round()`, and
    /// `.round()` absorbs the f64 epsilon, so the old code also returned all
    /// 1000 of these correctly (measured, not assumed). What the old code got
    /// wrong were the separator, sign and fail-open cases in AMOUNT_CASES
    /// above — those are the regressions. The float itself was a latent
    /// hazard rather than an active miscalculation in this range: it becomes
    /// active the moment anyone truncates instead of rounds, widens the range,
    /// or accepts a third decimal place.
    #[test]
    fn test_parse_amount_is_exact_across_all_cents() {
        for dollars in 0..10i64 {
            for cents in 0..100i64 {
                let s = format!("{dollars}.{cents:02}");
                assert_eq!(
                    parse_amount(&s), Some(dollars * 100 + cents),
                    "inexact conversion for {s}"
                );
            }
        }
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

    // Doc 08 §1: real Riverside GL headers (Debit/Credit) require the per-book
    // column mapping to extract amounts. Debit or Credit non-empty => amount.
    #[test]
    fn test_map_columns_riverside_gl() {
        let mut data = HashMap::new();
        data.insert("Date".to_string(), "06/06/2026".to_string());
        data.insert("Transaction Type".to_string(), "Bill Payment".to_string());
        data.insert("Num".to_string(), "10456".to_string());
        data.insert("Name".to_string(), "Acme Office Supplies Co.".to_string());
        data.insert("Memo".to_string(), "Payment - INV-1001, INV-1002".to_string());
        data.insert("Account".to_string(), "Accounts Payable".to_string());
        data.insert("Debit".to_string(), "471.25".to_string());
        data.insert("Credit".to_string(), "".to_string());

        let mut col_map = HashMap::new();
        col_map.insert("date".to_string(), "Date".to_string());
        col_map.insert("amount".to_string(), "Debit".to_string());
        col_map.insert("counterparty".to_string(), "Name".to_string());
        col_map.insert("description".to_string(), "Memo".to_string());
        col_map.insert("account_code".to_string(), "Account".to_string());

        let mapped = map_columns(&data, &col_map);
        assert_eq!(mapped.get("amount").unwrap(), "471.25");
        assert_eq!(parse_amount(mapped.get("amount").unwrap()).unwrap(), 47125);
        assert_eq!(mapped.get("counterparty").unwrap(), "Acme Office Supplies Co.");
        assert_eq!(parse_date(mapped.get("date").unwrap()).unwrap(), NaiveDate::from_ymd_opt(2026, 6, 6).unwrap());
    }

    // OFX timestamps are YYYYMMDDHHMMSS — parse_date must take the date portion.
    #[test]
    fn test_parse_date_ofx_timestamp() {
        assert_eq!(parse_date("20260606120000"), Some(NaiveDate::from_ymd_opt(2026, 6, 6).unwrap()));
        assert_eq!(parse_date("20260608120000"), Some(NaiveDate::from_ymd_opt(2026, 6, 8).unwrap()));
    }

    // Doc 08: double-entry GL debit+credit pairs collapse to one entity.
    #[test]
    fn test_dedupe_gl_pairs() {
        let d = NaiveDate::from_ymd_opt(2026, 6, 6).unwrap();
        let mk = |amt: i64| ExtractedEntity {
            entity_type: "gl_entry".into(),
            amount_cents: amt,
            transaction_date: Some(d),
            counterparty: Some("Acme".into()),
            description: None,
            gl_account_code: None,
            transaction_ref: None,
            currency: "USD".into(),
            page_number: 1,
            bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
            confidence: 1.0,
            source_format: "structured".into(),
        };
        let input = vec![mk(47125), mk(47125), mk(21500), mk(-21500)];
        let out = dedupe_gl_pairs(input);
        assert_eq!(out.len(), 2, "debit+credit pairs must collapse to one each");
    }

    // Doc 08: with a transaction ref present, dedup keys on the REF, so two
    // distinct entries sharing date+counterparty+amount are NOT collapsed.
    #[test]
    fn test_dedupe_gl_pairs_uses_ref() {
        let d = NaiveDate::from_ymd_opt(2026, 6, 6).unwrap();
        let mk = |amt: i64, rf: &str| ExtractedEntity {
            entity_type: "gl_entry".into(),
            amount_cents: amt,
            transaction_date: Some(d),
            counterparty: Some("Acme".into()),
            description: None,
            gl_account_code: None,
            transaction_ref: Some(rf.to_string()),
            currency: "USD".into(),
            page_number: 1,
            bbox: BoundingBox { x: 0.0, y: 0.0, width: 0.0, height: 0.0 },
            confidence: 1.0,
            source_format: "structured".into(),
        };
        // Two DIFFERENT refs, same date/counterparty/amount = 2 distinct entries.
        let input = vec![mk(47125, "A"), mk(47125, "B")];
        let out = dedupe_gl_pairs(input);
        assert_eq!(out.len(), 2, "distinct refs must not be collapsed");
        // Same ref, debit+credit legs = 1 entry.
        let input2 = vec![mk(47125, "X"), mk(47125, "X")];
        assert_eq!(dedupe_gl_pairs(input2).len(), 1, "same ref legs collapse to one");
    }
}
