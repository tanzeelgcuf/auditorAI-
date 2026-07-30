// services/ingestion/src/grpc/mod.rs
use tonic::{Request, Response, Status};
use uuid::Uuid;
use crate::ocr::{ExtractedEntity, OcrBackend, OcrError, ProcessDocumentRequest, ProcessDocumentResponse};
use crate::preprocess;
use crate::structured::detect_format;
use std::sync::Arc;
use nats::jetstream::JetStream;

pub mod ingestion_service {
    tonic::include_proto!("ingestion");
}

use ingestion_service::{
    ingestion_service_server::IngestionService,
    ProcessDocumentRequest as GrpcProcessRequest,
    ProcessDocumentResponse as GrpcProcessResponse,
    ExtractedEntity as GrpcExtractedEntity,
    BoundingBox as GrpcBoundingBox,
};

pub struct IngestionServiceImpl {
    ocr_backend: Arc<dyn OcrBackend>,
    structured_backends: std::collections::HashMap<String, Arc<dyn OcrBackend>>,
    js: JetStream,
}

impl IngestionServiceImpl {
    pub fn new(ocr_backend: Arc<dyn OcrBackend>, js: JetStream) -> Self {
        let mut structured = std::collections::HashMap::new();
        structured.insert("csv".to_string(), Arc::new(crate::ocr::structured::CsvParser::new(std::collections::HashMap::new())) as Arc<dyn OcrBackend>);
        structured.insert("xlsx".to_string(), Arc::new(crate::ocr::structured::XlsxParser::new(std::collections::HashMap::new())) as Arc<dyn OcrBackend>);
        structured.insert("ofx".to_string(), Arc::new(crate::ocr::structured::OfxParser) as Arc<dyn OcrBackend>);

        Self {
            ocr_backend,
            structured_backends: structured,
            js,
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
                x: e.bbox.x,
                y: e.bbox.y,
                width: e.bbox.width,
                height: e.bbox.height,
            }),
            confidence: e.confidence,
            source_format: e.source_format.clone(),
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

        let document_id = Uuid::parse_str(&req.document_id)
            .map_err(|_| Status::invalid_argument("invalid document_id"))?;
        let client_book_id = Uuid::parse_str(&req.client_book_id)
            .map_err(|_| Status::invalid_argument("invalid client_book_id"))?;

        let process_req = ProcessDocumentRequest {
            document_id,
            storage_key: req.storage_key.clone(),
            doc_type: req.doc_type.clone(),
            client_book_id,
        };

        // Detect format and route to appropriate backend
        // In real impl, download from S3 first and sniff content
        let format = detect_format(&req.storage_key, &[]).unwrap_or("ocr");

        let response = if let Some(backend) = self.structured_backends.get(format) {
            backend.process(&process_req).await
        } else {
            // Preprocess image for OCR
            // Download from S3, preprocess, then OCR
            self.ocr_backend.process(&process_req).await
        };

        match response {
            Ok(resp) => {
                let entities: Vec<GrpcExtractedEntity> = resp.entities.iter()
                    .map(Self::convert_entity)
                    .collect();

                // Publish completion event to NATS
                let event = serde_json::json!({
                    "document_id": req.document_id,
                    "client_book_id": req.client_book_id,
                    "entity_count": entities.len(),
                    "status": "completed"
                });
                let _ = self.js.publish("ingestion.completed", event.to_string().into());

                Ok(Response::new(GrpcProcessResponse { entities }))
            }
            Err(e) => {
                let _ = self.js.publish("ingestion.failed", format!("{}: {}", req.document_id, e).into());
                Err(Status::internal(e.to_string()))
            }
        }
    }

    async fn get_processing_status(
        &self,
        request: Request<ingestion_service::StatusRequest>,
    ) -> Result<Response<ingestion_service::StatusResponse>, Status> {
        let req = request.into_inner();
        // In real impl, check status from DB or cache
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
    use crate::ocr::{DoctrBackend, OcrBackend};

    #[tokio::test]
    async fn test_format_detection() {
        assert_eq!(crate::structured::detect_format("test.pdf", &[]).unwrap(), "ocr");
        assert_eq!(crate::structured::detect_format("test.csv", &[]).unwrap(), "csv");
        assert_eq!(crate::structured::detect_format("test.ofx", &[]).unwrap(), "ofx");
    }
}