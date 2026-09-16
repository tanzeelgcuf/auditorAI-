// services/ingestion/src/main.rs
#![deny(clippy::unwrap_used)]
mod bbox;
mod grpc;
mod ocr;
// Cargo.toml has always SAID the preprocessing stages are "compiled only
// under the feature", but `mod preprocess;` was unconditional, so the no-op
// placeholder pipeline compiled as dead code into every default build —
// 8+ of the 21 warnings observed 2026-09-17. Gating the mod makes the
// documented intent real: default builds contain no preprocessing code at
// all. Remove the gate the day a real implementation is wired into the OCR
// path.
#[cfg(feature = "enhance")]
mod preprocess;
mod telemetry;
use clap::Parser;
use std::sync::Arc;
use tonic::transport::Server;
use tracing::info;

use crate::grpc::ingestion_service::ingestion_service_server::IngestionServiceServer;
use crate::grpc::IngestionServiceImpl;
use crate::ocr::OcrBackend;

#[derive(Parser, Debug)]
#[command(name = "ingestion", version, about = "AI Auditor Ingestion Service")]
struct Args {
    #[arg(long, env = "GRPC_ADDR", default_value = "[::]:50051")]
    grpc_addr: String,

    #[arg(
        long,
        env = "OCR_SIDECAR_URL",
        default_value = "http://ocr-sidecar:8000"
    )]
    ocr_sidecar_url: String,

    #[arg(long, env = "NATS_URL", default_value = "nats://nats:4222")]
    nats_url: String,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    // GlitchTip error reporting — no-ops when GLITCHTIP_DSN is unset, so dev
    // runs behave identically to today.
    crate::telemetry::init_glitchtip();
    crate::telemetry::install_panic_hook();

    let args = Args::parse();

    info!("Starting ingestion service on {}", args.grpc_addr);

    // Initialize OCR backend (docTR sidecar)
    let ocr_backend: Arc<dyn OcrBackend> =
        Arc::new(crate::ocr::DoctrBackend::new(&args.ocr_sidecar_url).await?);

    // Initialize NATS connection (async-nats, the maintained successor client)
    let nc = async_nats::connect(&args.nats_url).await?;
    let js = async_nats::jetstream::new(nc);

    // Create service implementation
    let svc = IngestionServiceImpl::new(ocr_backend, js).await;

    // Start gRPC server
    let addr = args.grpc_addr.parse()?;
    Server::builder()
        .add_service(IngestionServiceServer::new(svc))
        .serve(addr)
        .await?;

    Ok(())
}
