// services/ingestion/src/main.rs
mod grpc;
mod preprocess;
mod ocr;
mod bbox;

use std::sync::Arc;
use tonic::transport::Server;
use tracing::{info, error};
use clap::Parser;

use crate::grpc::{IngestionServiceImpl, ingestion_service_server::IngestionServiceServer};
use crate::ocr::OcrBackend;

#[derive(Parser, Debug)]
#[command(name = "ingestion", version, about = "AI Auditor Ingestion Service")]
struct Args {
    #[arg(long, env = "GRPC_ADDR", default_value = "[::]:50051")]
    grpc_addr: String,

    #[arg(long, env = "OCR_SIDECAR_URL", default_value = "http://ocr-sidecar:8000")]
    ocr_sidecar_url: String,

    #[arg(long, env = "NATS_URL", default_value = "nats://nats:4222")]
    nats_url: String,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let args = Args::parse();

    info!("Starting ingestion service on {}", args.grpc_addr);

    // Initialize OCR backend (docTR sidecar)
    let ocr_backend: Arc<dyn OcrBackend> = Arc::new(
        crate::ocr::DoctrBackend::new(&args.ocr_sidecar_url).await?
    );

    // Initialize NATS connection
    let nc = nats::connect(&args.nats_url)?;
    let js = nc.jetstream()?;

    // Create service implementation
    let svc = IngestionServiceImpl::new(ocr_backend, js);

    // Start gRPC server
    let addr = args.grpc_addr.parse()?;
    Server::builder()
        .add_service(IngestionServiceServer::new(svc))
        .serve(addr)
        .await?;

    Ok(())
}