use super::{ExtractedEntity, OcrBackend, OcrError, ProcessDocumentRequest, ProcessDocumentResponse, BoundingBox};
use async_trait::async_trait;
use chrono::NaiveDate;
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::sync::Arc;

#[derive(Debug, Serialize)]
struct DoctrRequest {
    storage_key: String,
    doc_type: String,
}

#[derive(Debug, Deserialize)]
struct DoctrResponse {
    pages: Vec<DoctrPage>,
}

#[derive(Debug, Deserialize)]
struct DoctrPage {
    page_number: i32,
    width: f32,
    height: f32,
    blocks: Vec<DoctrBlock>,
}

#[derive(Debug, Deserialize)]
struct DoctrBlock {
    geometry: [[f32; 2]; 4],
    lines: Vec<DoctrLine>,
    confidence: f32,
}

#[derive(Debug, Deserialize)]
struct DoctrLine {
    geometry: [[f32; 2]; 4],
    words: Vec<DoctrWord>,
    confidence: f32,
}

#[derive(Debug, Deserialize)]
struct DoctrWord {
    value: String,
    confidence: f32,
    geometry: [[f32; 2]; 4],
}

pub struct DoctrBackend {
    client: Client,
    base_url: String,
    s3_client: Arc<aws_sdk_s3::Client>,
    bucket: String,
}

impl DoctrBackend {
    pub async fn new(sidecar_url: &str) -> Result<Self, OcrError> {
        let client = Client::new();

        let s3_client = Arc::new(crate::ocr::structured::build_s3_client().await);
        let bucket = std::env::var("S3_BUCKET").unwrap_or_else(|_| "ai-auditor".to_string());

        Ok(Self {
            client,
            base_url: sidecar_url.trim_end_matches('/').to_string(),
            s3_client,
            bucket,
        })
    }

    fn generate_presigned_url(&self, key: &str) -> Result<String, OcrError> {
        Ok(format!("{}/{}", self.base_url.replace("ocr-sidecar:8000", "minio:9000"), key))
    }

    fn normalize_bbox(&self, geometry: [[f32; 2]; 4], page_width: f32, page_height: f32) -> BoundingBox {
        let xs: Vec<f32> = geometry.iter().map(|p| p[0]).collect();
        let ys: Vec<f32> = geometry.iter().map(|p| p[1]).collect();

        let min_x = xs.iter().cloned().fold(f32::INFINITY, f32::min);
        let max_x = xs.iter().cloned().fold(f32::NEG_INFINITY, f32::max);
        let min_y = ys.iter().cloned().fold(f32::INFINITY, f32::min);
        let max_y = ys.iter().cloned().fold(f32::NEG_INFINITY, f32::max);

        BoundingBox {
            x: min_x / page_width,
            y: min_y / page_height,
            width: (max_x - min_x) / page_width,
            height: (max_y - min_y) / page_height,
        }
    }

    fn classify_entity_type(&self, text: &str, doc_type: &str) -> String {
        match doc_type {
            "invoice" => "invoice_line_item",
            "bank_statement" => "bank_transaction",
            "gl_export" => "gl_entry",
            _ => "invoice_line_item",
        }
        .to_string()
    }

    /// Extract amount from text via char-by-char scanning.
    ///
    /// Only CURRENCY-SHAPED values are accepted — an optional `$`, digits with
    /// optional comma thousands, and EXACTLY two decimal digits (`150.00`).
    /// ZIPs, dates, and invoice numbers (97401, 2026, 9042) carry no cents and
    /// are filtered out BEFORE they reach the LLM. Without this, a page's raw
    /// OCR lines ("2210 Meadowbrook Ave, Eugene, OR 97401") become noisy
    /// pseudo-entities that blow up the extraction prompt and time out the
    /// model (Stress Set #2: 13 entities → 5×$150 + 8 garbage). The model's
    /// real job — pick WHICH currency value is the total — is preserved; this
    /// only removes the deterministically-non-currency noise.
    fn extract_amount(text: &str) -> Option<i64> {
        let mut current = String::new();
        let mut candidates: Vec<f64> = Vec::new();

        for c in text.chars() {
            if c.is_ascii_digit() || c == '.' || c == '-' || c == ',' || c == '$' {
                current.push(c);
            } else if !current.is_empty() {
                if let Some(v) = Self::parse_currency(&current) {
                    candidates.push(v);
                }
                current.clear();
            }
        }
        if !current.is_empty() {
            if let Some(v) = Self::parse_currency(&current) {
                candidates.push(v);
            }
        }

        candidates
            .iter()
            .max_by(|a, b| a.abs().partial_cmp(&b.abs()).unwrap_or(std::cmp::Ordering::Equal))
            .map(|v| (v * 100.0).round() as i64)
    }

    /// Parse a candidate as a currency value: optional `$`, digits with comma
    /// thousands, and EXACTLY two decimal digits. Returns None for anything not
    /// shaped like a dollar amount (ZIPs, dates, IDs, bare integers).
    fn parse_currency(candidate: &str) -> Option<f64> {
        let s = candidate.trim();
        let neg = s.starts_with('-');
        let digits_only = |x: &str| {
            let d: String = x.chars().filter(|c| *c != ',' && *c != '$').collect();
            d
        };
        let core = digits_only(s.trim_start_matches(['-', '$']));
        // Must have exactly two decimal places (currency cents), no more.
        if core.contains('.') {
            let (whole, frac) = core.split_once('.')?;
            if !frac.is_empty() && frac.len() == 2 && frac.chars().all(|c| c.is_ascii_digit())
                && !whole.is_empty() && whole.chars().all(|c| c.is_ascii_digit())
            {
                let v: f64 = core.parse().ok()?;
                return Some(if neg { -v } else { v });
            }
        }
        None
    }

    /// Extract date from text via sliding window pattern matching.
    fn extract_date(text: &str) -> Option<NaiveDate> {
        let chars: Vec<char> = text.chars().collect();
        if chars.len() < 8 {
            return None;
        }

        // Combinations of separators and lengths to try
        let patterns: &[(usize, usize, &str, &str)] = &[
            (10, 10, "%Y-%m-%d", "YYYY-MM-DD"),
            (10, 10, "%m/%d/%Y", "MM/DD/YYYY"),
            (10, 10, "%d/%m/%Y", "DD/MM/YYYY"),
            (8, 8, "%Y%m%d", "YYYYMMDD"),
            (8, 8, "%m/%d/%y", "MM/DD/YY"),
            (8, 8, "%d/%m/%y", "DD/MM/YY"),
            (10, 10, "%m-%d-%Y", "MM-DD-YYYY"),
            (10, 10, "%d.%m.%Y", "DD.MM.YYYY"),
        ];

        for &(min_len, max_len, fmt, _) in patterns {
            let end = chars.len().saturating_sub(min_len).min(chars.len());
            for i in 0..=end {
                let slice_end = (i + max_len).min(chars.len());
                if slice_end - i < min_len {
                    continue;
                }
                let slice: String = chars[i..slice_end].iter().collect();
                if let Ok(d) = NaiveDate::parse_from_str(&slice, fmt) {
                    // Reject misaligned windows: chrono accepts a truncated
                    // slice as a valid date (" 07/03/202" parses as year 202
                    // when the real "07/03/2026" begins one char later), and
                    // first-match returns it before the aligned window. A real
                    // date is not bounded on either side by another digit or a
                    // date separator.
                    let before = i.checked_sub(1).map(|j| chars[j]).unwrap_or(' ');
                    let after = chars.get(slice_end).copied().unwrap_or(' ');
                    let is_date_char = |c: char| c.is_ascii_digit() || c == '/' || c == '-' || c == '.';
                    if !is_date_char(before) && !is_date_char(after) {
                        return Some(d);
                    }
                }
            }
        }

        None
    }

    /// Join a line's words with spaces into display text.
    fn line_text(line: &DoctrLine) -> String {
        line.words
            .iter()
            .map(|w| w.value.as_str())
            .collect::<Vec<_>>()
            .join(" ")
    }

    /// Attach a transaction date to an amount line via deterministic proximity.
    ///
    /// Order:
    /// 1. The amount line's own text (extract_date already matches a line like
    ///    "Invoice #: SLG-771 Date: 07/03/2026" directly).
    /// 2. Otherwise, scan the block's other lines for a date pattern and pick
    ///    the NEAREST by vertical geometry (y distance), preferring a line that
    ///    carries a "Date:" label when such exists.
    /// 3. None if no date exists in the block.
    ///
    /// Generalizes across real layouts: QBO exports and firm invoices put dates
    /// in varied positions (header, side, near the total). Proximity + label
    /// preference is the general rule; this invoice's exact layout is not baked in.
    fn attach_date(amount_text: &str, amount_y: f32, block_lines: &[(String, f32)]) -> Option<NaiveDate> {
        if let Some(d) = Self::extract_date(amount_text) {
            return Some(d);
        }

        let mut candidates: Vec<(f32, NaiveDate)> = block_lines
            .iter()
            .filter(|(t, _)| t.as_str() != amount_text)
            .filter_map(|(t, y)| Self::extract_date(t).map(|d| (*y, d)))
            .collect();

        if candidates.is_empty() {
            return None;
        }

        // Prefer a line carrying a "Date:" label, as the most explicit signal.
        let labeled: Vec<(f32, NaiveDate)> = block_lines
            .iter()
            .filter(|(t, _)| t.contains("Date:") && t.as_str() != amount_text)
            .filter_map(|(t, y)| Self::extract_date(t).map(|d| (*y, d)))
            .collect();
        if !labeled.is_empty() {
            candidates = labeled;
        }

        candidates.sort_by(|a, b| {
            let da = (a.0 - amount_y).abs();
            let db = (b.0 - amount_y).abs();
            da.partial_cmp(&db).unwrap_or(std::cmp::Ordering::Equal)
        });
        candidates.first().map(|(_, d)| *d)
    }
}

#[async_trait]
impl OcrBackend for DoctrBackend {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        // The sidecar downloads the object itself via storage_key (S3-compatible).
        let resp = self
            .client
            .post(&format!("{}/ocr/process", self.base_url))
            .json(&DoctrRequest {
                storage_key: request.storage_key.clone(),
                doc_type: request.doc_type.clone(),
            })
            .send()
            .await
            .map_err(|e| OcrError::SidecarError(e.to_string()))?;

        if !resp.status().is_success() {
            return Err(OcrError::ProcessingFailed(resp.text().await.unwrap_or_default()));
        }

        let doctr_resp: DoctrResponse = resp
            .json()
            .await
            .map_err(|e| OcrError::SidecarError(e.to_string()))?;

        let mut entities = Vec::new();

        for page in doctr_resp.pages {
            for block in page.blocks {
                // Collect all raw lines (text + vertical position) in the block
                // so an amount line can pick up a date that lives on a different
                // line (header, side, near the total). Narrow per-line scanning
                // lost the date entirely once the currency filter dropped the
                // date-bearing line's non-currency "amount".
                let block_lines: Vec<(String, f32)> = block
                    .lines
                    .iter()
                    .map(|l| (DoctrBackend::line_text(l), l.geometry[0][1]))
                    .collect();

                for line in block.lines {
                    let text = DoctrBackend::line_text(&line);

                    if let Some(amount) = DoctrBackend::extract_amount(&text) {
                        let bbox = self.normalize_bbox(line.geometry, page.width, page.height);
                        let date = DoctrBackend::attach_date(&text, line.geometry[0][1], &block_lines);

                        entities.push(ExtractedEntity {
                            entity_type: self.classify_entity_type(&text, &request.doc_type),
                            amount_cents: amount,
                            currency: "USD".to_string(),
                            transaction_date: date,
                            counterparty: None,
                            description: Some(text),
                            gl_account_code: None,
                            transaction_ref: None,
                            page_number: page.page_number,
                            bbox,
                            confidence: line.confidence,
                            source_format: "ocr".to_string(),
                        });
                    }
                }
            }
        }

        Ok(ProcessDocumentResponse { entities })
    }

    fn name(&self) -> &'static str {
        "docTR"
    }
}


#[cfg(test)]
mod tests {
    use super::DoctrBackend;

    fn amt(text: &str) -> Option<i64> {
        DoctrBackend::extract_amount(text)
    }

    #[test]
    fn currency_values_accepted() {
        assert_eq!(amt("$150.00"), Some(15000));
        assert_eq!(amt("Total Due: $150.00"), Some(15000));
        assert_eq!(amt("1,234.56"), Some(123456));
        assert_eq!(amt("-150.00"), Some(-15000));
        assert_eq!(amt("850.00 line plus 900.00 total"), Some(90000));
    }

    #[test]
    fn non_currency_noise_filtered() {
        // Stress Set #2 noise: ZIPs, dates, invoice numbers have no cents.
        assert_eq!(amt("2210 Meadowbrook Ave, Eugene, OR 97401"), None);
        assert_eq!(amt("Invoice #: SLG-771 Date: 07/03/2026"), None);
        assert_eq!(amt("PO Box 4471, Eugene OR 97440"), None);
        assert_eq!(amt("Invoice No: SLM-9042"), None);
        assert_eq!(amt("1"), None);
        assert_eq!(amt("2026"), None);
    }

    #[test]
    fn bare_integer_with_dollar_no_cents_filtered() {
        // "$150" without cents is not a currency amount on an invoice page.
        assert_eq!(amt("$150"), None);
    }

    fn block(rows: &[(&str, f32)]) -> Vec<(String, f32)> {
        rows.iter().map(|(t, y)| (t.to_string(), *y)).collect()
    }

    #[test]
    fn date_attached_from_other_line_in_block() {
        // "$150.00" line has no date itself; the header line in the same block does.
        let lines = block(&[
            ("Invoice #: SLG-771 Date: 07/03/2026", 0.10),
            ("Consulting services", 0.20),
            ("$150.00", 0.30),
        ]);
        let d = DoctrBackend::attach_date("$150.00", 0.30, &lines);
        assert_eq!(d, Some(chrono::NaiveDate::from_ymd_opt(2026, 7, 3).unwrap()));
    }

    #[test]
    fn no_date_in_block_yields_none() {
        let lines = block(&[
            ("Consulting services", 0.20),
            ("$150.00", 0.30),
        ]);
        assert_eq!(DoctrBackend::attach_date("$150.00", 0.30, &lines), None);
    }

    #[test]
    fn proximity_picks_nearest_of_two_dates() {
        // Two date lines; the closer one (0.25) must win over 0.60.
        let lines = block(&[
            ("Invoice date: 07/03/2026", 0.25),
            ("Terms Net 30 due 08/02/2026", 0.60),
            ("$150.00", 0.30),
        ]);
        let d = DoctrBackend::attach_date("$150.00", 0.30, &lines);
        assert_eq!(d, Some(chrono::NaiveDate::from_ymd_opt(2026, 7, 3).unwrap()));
    }

    #[test]
    fn date_label_preferred_over_nearer_unlabeled() {
        // A "Date:"-labeled line farther away beats an unlabeled nearer one.
        let lines = block(&[
            ("Terms Net 30 due 08/02/2026", 0.31),
            ("Date: 07/03/2026", 0.60),
            ("$150.00", 0.32),
        ]);
        let d = DoctrBackend::attach_date("$150.00", 0.32, &lines);
        assert_eq!(d, Some(chrono::NaiveDate::from_ymd_opt(2026, 7, 3).unwrap()));
    }
}
