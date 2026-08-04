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
    fn extract_date(&self, text: &str) -> Option<NaiveDate> {
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
                    return Some(d);
                }
            }
        }

        None
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
                for line in block.lines {
                    let text: String = line
                        .words
                        .iter()
                        .map(|w| w.value.as_str())
                        .collect::<Vec<_>>()
                        .join(" ");

                    if let Some(amount) = DoctrBackend::extract_amount(&text) {
                        let bbox = self.normalize_bbox(line.geometry, page.width, page.height);

                        entities.push(ExtractedEntity {
                            entity_type: self.classify_entity_type(&text, &request.doc_type),
                            amount_cents: amount,
                            currency: "USD".to_string(),
                            transaction_date: self.extract_date(&text),
                            counterparty: None,
                            description: Some(text.clone()),
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
}
