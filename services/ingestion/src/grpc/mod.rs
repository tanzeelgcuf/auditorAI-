use crate::ocr::{
    DetectedFormat, ExtractedEntity, FormatDetector, OcrBackend, OcrError,
    ProcessDocumentRequest,
};
use std::sync::Arc;

use async_nats::jetstream::Context as JetStream;
use tonic::{Request, Response, Status};

pub mod ingestion_service {
    tonic::include_proto!("ingestion");
}

use ingestion_service::{
    ingestion_service_server::IngestionService,
    BoundingBox as GrpcBoundingBox,
    ExtractedEntity as GrpcExtractedEntity,
    ProcessDocumentRequest as GrpcProcessRequest,
    ProcessDocumentResponse as GrpcProcessResponse,
};

/// True if a path's extension is a definitive OCR media type (PDF/image) that
/// must not be content-sniffed.
fn is_image_or_pdf(path: &str) -> bool {
    let lower = path.to_lowercase();
    lower.ends_with(".pdf")
        || lower.ends_with(".png")
        || lower.ends_with(".jpg")
        || lower.ends_with(".jpeg")
        || lower.ends_with(".tiff")
        || lower.ends_with(".bmp")
}

fn ocr_error_to_status(e: OcrError) -> Status {
    match e {
        OcrError::NotFound(msg) => Status::not_found(msg),
        OcrError::UnsupportedFormat(msg) => Status::invalid_argument(msg),
        OcrError::ProcessingFailed(msg) => Status::internal(msg),
        OcrError::S3Error(msg) => Status::internal(format!("storage error: {msg}")),
        OcrError::SidecarError(msg) => Status::unavailable(format!("sidecar unavailable: {msg}")),
        OcrError::ParsingError(msg) => Status::invalid_argument(format!("parse error: {msg}")),
    }
}

pub struct IngestionServiceImpl {
    ocr_backend: Arc<dyn OcrBackend>,
    structured_backends: std::collections::HashMap<String, Arc<dyn OcrBackend>>,
    js: JetStream,
    s3_client: Arc<aws_sdk_s3::Client>,
    bucket: String,
}

impl IngestionServiceImpl {
    pub async fn new(ocr_backend: Arc<dyn OcrBackend>, js: JetStream) -> Self {
        let s3_client = Arc::new(crate::ocr::structured::build_s3_client().await);
        let bucket = std::env::var("S3_BUCKET").unwrap_or_else(|_| "ai-auditor".to_string());

        let mut structured = std::collections::HashMap::new();
        structured.insert(
            "csv".to_string(),
            Arc::new(crate::ocr::structured::CsvParser::new(
                std::collections::HashMap::new(),
                s3_client.clone(),
                bucket.clone(),
            )) as Arc<dyn OcrBackend>,
        );
        structured.insert(
            "xlsx".to_string(),
            Arc::new(crate::ocr::structured::XlsxParser::new(
                std::collections::HashMap::new(),
                s3_client.clone(),
                bucket.clone(),
            )) as Arc<dyn OcrBackend>,
        );
        structured.insert(
            "ofx".to_string(),
            Arc::new(crate::ocr::structured::OfxParser::new(
                s3_client.clone(),
                bucket.clone(),
            )) as Arc<dyn OcrBackend>,
        );

        Self {
            ocr_backend,
            structured_backends: structured,
            js,
            s3_client,
            bucket,
        }
    }

    fn backend_key(format: DetectedFormat) -> Option<&'static str> {
        match format {
            DetectedFormat::Csv => Some("csv"),
            DetectedFormat::Xlsx => Some("xlsx"),
            DetectedFormat::Ofx => Some("ofx"),
            DetectedFormat::Ocr => None,
        }
    }

    fn convert_entity(e: &ExtractedEntity) -> GrpcExtractedEntity {
        GrpcExtractedEntity {
            entity_type: e.entity_type.clone(),
            amount_cents: e.amount_cents,
            currency: e.currency.clone(),
            transaction_date: e.transaction_date.map(|d| d.to_string()).unwrap_or_default(),
            counterparty: e.counterparty.clone().unwrap_or_default(),
            description: e.description.clone().unwrap_or_default(),
            gl_account_code: e.gl_account_code.clone().unwrap_or_default(),
            page_number: e.page_number,
            bbox: Some(GrpcBoundingBox {
                x: e.bbox.x as f64,
                y: e.bbox.y as f64,
                width: e.bbox.width as f64,
                height: e.bbox.height as f64,
            }),
            confidence: e.confidence as f64,
            source_format: e.source_format.clone(),
            entity_subtype: String::new(),
            transaction_ref: e.transaction_ref.clone().unwrap_or_default(),
        }
    }
}

#[tonic::async_trait]
impl IngestionService for IngestionServiceImpl {
    async fn process_document(
        &self,
        request: Request<GrpcProcessRequest>,
    ) -> Result<Response<GrpcProcessResponse>, Status> {
        let req = request.into_inner();

        let document_id = uuid::Uuid::parse_str(&req.document_id)
            .map_err(|_| Status::invalid_argument("invalid document_id"))?;
        let client_book_id = uuid::Uuid::parse_str(&req.client_book_id)
            .map_err(|_| Status::invalid_argument("invalid client_book_id"))?;

        let process_req = ProcessDocumentRequest {
            document_id,
            storage_key: req.storage_key.clone(),
            doc_type: req.doc_type.clone(),
            client_book_id,
            column_map: req.column_map.clone(),
        };

        // Detect format by extension first; content-sniff ONLY for extensions that
        // are not a definitive OCR media type. A .pdf/.png/.jpg is unambiguously OCR
        // and must not be sniffed into CSV by a stray comma in the PDF binary.
        let mut format = FormatDetector::from_extension(&req.storage_key);
        if format == DetectedFormat::Ocr && !is_image_or_pdf(&req.storage_key) {
            // download first bytes for content sniff
            match crate::ocr::structured::download_from_s3(
                &self.s3_client,
                &self.bucket,
                &req.storage_key,
            )
            .await
            {
                Ok(data) => {
                    let sniff_len = data.len().min(2048);
                    format = FormatDetector::from_content(&data[..sniff_len]);
                }
                Err(e) => return Err(ocr_error_to_status(e)),
            }
        }

        // Route to backend. Structured formats use a per-request backend so the
        // book's CSV column mapping (doc 08 §1) is applied; OCR uses the singleton.
        let response = match format {
            DetectedFormat::Csv => {
                let backend = crate::ocr::structured::CsvParser::new(
                    process_req.column_map.clone(), self.s3_client.clone(), self.bucket.clone());
                backend.process(&process_req).await.map_err(ocr_error_to_status)?
            }
            DetectedFormat::Xlsx => {
                let backend = crate::ocr::structured::XlsxParser::new(
                    process_req.column_map.clone(), self.s3_client.clone(), self.bucket.clone());
                backend.process(&process_req).await.map_err(ocr_error_to_status)?
            }
            DetectedFormat::Ofx => {
                // OFX is structured (STMTTRN blocks), NOT OCR. It was falling
                // through to the OCR sidecar (the `_` arm), which returns
                // Not Found — the structured ofx parser was never routed to.
                // Prompt B wiring-first catch.
                let backend = crate::ocr::structured::OfxParser::new(
                    self.s3_client.clone(), self.bucket.clone());
                backend.process(&process_req).await.map_err(ocr_error_to_status)?
            }
            _ => {
                self.ocr_backend.process(&process_req).await.map_err(ocr_error_to_status)?
            }
        };

        let entities: Vec<GrpcExtractedEntity> =
            response.entities.iter().map(Self::convert_entity).collect();

        // Publish completion event
        let event = serde_json::json!({
            "document_id": req.document_id,
            "client_book_id": req.client_book_id,
            "entity_count": entities.len(),
            "status": "completed"
        });
        let _ = self.js.publish("ingestion.completed", event.to_string().into()).await;

        Ok(Response::new(GrpcProcessResponse { entities }))
    }

    async fn get_processing_status(
        &self,
        request: Request<ingestion_service::StatusRequest>,
    ) -> Result<Response<ingestion_service::StatusResponse>, Status> {
        let req = request.into_inner();
        Ok(Response::new(ingestion_service::StatusResponse {
            document_id: req.document_id,
            status: "completed".to_string(),
            progress: 100,
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_format_detection() {
        assert_eq!(FormatDetector::from_extension("test.pdf"), DetectedFormat::Ocr);
        assert_eq!(FormatDetector::from_extension("test.csv"), DetectedFormat::Csv);
        assert_eq!(FormatDetector::from_extension("test.ofx"), DetectedFormat::Ofx);
        assert_eq!(FormatDetector::from_extension("test.xlsx"), DetectedFormat::Xlsx);
        assert_eq!(FormatDetector::from_extension("test.xls"), DetectedFormat::Xlsx);
        assert_eq!(FormatDetector::from_extension("test.qfx"), DetectedFormat::Ofx);
    }

    #[test]
    fn test_content_detection_ofx() {
        assert_eq!(
            FormatDetector::from_content(b"OFXHEADER:100\nDATA:OFXSGML"),
            DetectedFormat::Ofx
        );
        assert_eq!(
            FormatDetector::from_content(b"<?xml version=\"1.0\"?>\n<OFX>"),
            DetectedFormat::Ofx
        );
    }

    #[test]
    fn test_content_detection_csv() {
        assert_eq!(
            FormatDetector::from_content(b"date,amount,description\n2024-01-15,100,foo"),
            DetectedFormat::Csv
        );
    }

    #[test]
    fn test_error_to_status_not_found() {
        let e = OcrError::NotFound("doc missing".into());
        let s = ocr_error_to_status(e);
        assert_eq!(s.code(), tonic::Code::NotFound);
    }

    #[test]
    fn test_error_to_status_sidecar() {
        let e = OcrError::SidecarError("connection refused".into());
        let s = ocr_error_to_status(e);
        assert_eq!(s.code(), tonic::Code::Unavailable);
    }

    #[test]
    fn test_convert_entity() {
        let e = ExtractedEntity {
            entity_type: "invoice_line_item".into(),
            amount_cents: 15000,
            currency: "USD".into(),
            transaction_date: chrono::NaiveDate::from_ymd_opt(2024, 1, 15),
            counterparty: Some("Acme Corp".into()),
            description: Some("Widgets".into()),
            gl_account_code: Some("4000".into()),
            transaction_ref: Some("10456".into()),
            page_number: 1,
            bbox: crate::ocr::BoundingBox {
                x: 0.1,
                y: 0.2,
                width: 0.3,
                height: 0.4,
            },
            confidence: 0.95,
            source_format: "ocr".into(),
        };
        let g = IngestionServiceImpl::convert_entity(&e);
        assert_eq!(g.amount_cents, 15000);
        assert_eq!(g.description, "Widgets");
        let bx = g.bbox.unwrap().x;
        assert!((bx - 0.1).abs() < 1e-5, "bbox.x = {bx}, want ~0.1");
    }
}
