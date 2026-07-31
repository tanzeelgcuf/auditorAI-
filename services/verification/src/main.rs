// services/verification/src/main.rs
#![deny(clippy::unwrap_used)]

mod grpc;
mod decimal_math;
mod zen;

use std::sync::Arc;
use tonic::transport::Server;
use tracing::{info, error};
use clap::Parser;

use crate::grpc::{VerificationServiceImpl};
use crate::grpc::verification_service::verification_service_server::VerificationServiceServer;
use crate::zen::RuleEngine;

#[derive(Parser, Debug)]
#[command(name = "verification", version, about = "AI Auditor Verification Service")]
struct Args {
    #[arg(long, env = "GRPC_ADDR", default_value = "[::]:50052")]
    grpc_addr: String,

    #[arg(long, env = "ZEN_DECISION_GRAPH_PATH", default_value = "./decision-graphs/gl_reconciliation.json")]
    decision_graph_path: String,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let args = Args::parse();

    info!("Starting verification service on {}", args.grpc_addr);

    // Initialize RuleEngine with decision graph (synchronous load)
    let rule_engine = Arc::new(
        RuleEngine::new(&args.decision_graph_path)
            .map_err(|e| {
                error!("Failed to load decision graph: {}", e);
                e
            })?
    );

    info!(
        "Loaded decision graph: rule_id={}, rule_version={}",
        rule_engine.rule_id,
        rule_engine.rule_version,
    );

    // Create service implementation
    let svc = VerificationServiceImpl::new(rule_engine);

    // Start gRPC server
    let addr = args.grpc_addr.parse()?;
    Server::builder()
        .add_service(VerificationServiceServer::new(svc))
        .serve(addr)
        .await?;

    Ok(())
}
