// services/ingestion/src/ocr/doctr.rs
use super::{ExtractedEntity, OcrBackend, OcrError, ProcessDocumentRequest, ProcessDocumentResponse, BoundingBox};
use async_trait::async_trait;
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use uuid::Uuid;
use chrono::NaiveDate;

#[derive(Debug, Serialize)]
struct DoctrRequest {
    document_url: String,
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
    geometry: [[f32; 2]; 4],  // 4 corners: [[x1,y1], [x2,y2], [x3,y3], [x4,y4]]
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

        // Initialize S3 client for reading documents
        let config = aws_config::load_from_env().await;
        let s3_client = Arc::new(aws_sdk_s3::Client::new(&config));
        let bucket = std::env::var("S3_BUCKET").unwrap_or_else(|_| "ai-auditor".to_string());

        Ok(Self {
            client,
            base_url: sidecar_url.trim_end_matches('/').to_string(),
            s3_client,
            bucket,
        })
    }

    fn generate_presigned_url(&self, key: &str) -> Result<String, OcrError> {
        // In real implementation, use s3_client.create_presigned_url
        // For now, return a placeholder
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
        }.to_string()
    }

    fn extract_amount(&self, text: &str) -> Option<i64> {
        // Simple amount extraction - real impl would be more sophisticated
        let re = regex::Regex::new(r"[\$€£]?\s?(\d{1,3}(?:[,\s]\d{3})*(?:\.\d{2})?)").ok()?;
        re.captures(text).and_then(|c| c.get(1)).and_then(|m| {
            m.as_str().replace(",", "").replace(" ", "").parse::<f64>().ok()
                .map(|v| (v * 100.0).round() as i64)
        })
    }

    fn extract_date(&self, text: &str) -> Option<NaiveDate> {
        // Simple date extraction
        let re = regex::Regex::new(r"(\d{1,2}[/-]\d{1,2}[/-]\d{2,4})").ok()?;
        re.captures(text).and_then(|c| c.get(1)).and_then(|m| {
            chrono::NaiveDate::parse_from_str(m.as_str(), "%m/%d/%Y")
                .or_else(|_| chrono::NaiveDate::parse_from_str(m.as_str(), "%d/%m/%Y"))
                .ok()
        })
    }
}

#[async_trait]
impl OcrBackend for DoctrBackend {
    async fn process(&self, request: &ProcessDocumentRequest) -> Result<ProcessDocumentResponse, OcrError> {
        // Get presigned URL for document
        let doc_url = self.generate_presigned_url(&request.storage_key)?;

        // Call docTR sidecar
        let resp = self.client
            .post(&format!("{}/ocr", self.base_url))
            .json(&DoctrRequest {
                document_url: doc_url,
                doc_type: request.doc_type.clone(),
            })
            .send()
            .await
            .map_err(|e| OcrError::SidecarError(e.to_string()))?;

        if !resp.status().is_success() {
            return Err(OcrError::ProcessingFailed(resp.text().await.unwrap_or_default()));
        }

        let doctr_resp: DoctrResponse = resp.json().await
            .map_err(|e| OcrError::SidecarError(e.to_string()))?;

        // Convert docTR output to our entity format
        let mut entities = Vec::new();

        for page in doctr_resp.pages {
            for block in page.blocks {
                for line in block.lines {
                    let text: String = line.words.iter().map(|w| w.value.as_str()).collect::<Vec<_>>().join(" ");

                    if let Some(amount) = self.extract_amount(&text) {
                        let bbox = self.normalize_bbox(line.geometry, page.width, page.height);

                        entities.push(ExtractedEntity {
                            entity_type: self.classify_entity_type(&text, &request.doc_type),
                            amount_cents: amount,
                            currency: "USD".to_string(), // TODO: detect from text
                            transaction_date: self.extract_date(&text),
                            counterparty: None, // TODO: NER for counterparty
                            description: Some(text.clone()),
                            gl_account_code: None,
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