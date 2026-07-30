// services/verification/src/grpc/mod.rs
use tonic::{Request, Response, Status};
use uuid::Uuid;
use std::sync::Arc;

use crate::decimal_math;
use crate::zen::{self, ReconciliationInput, ReconciliationOutput, ZenEngine};

pub mod verification_service {
    tonic::include_proto!("verification");
}

use verification_service::{
    verification_service_server::VerificationService,
    ReconciliationRequest as GrpcReconciliationRequest,
    ReconciliationResult as GrpcReconciliationResult,
};

pub struct VerificationServiceImpl {
    zen_engine: Arc<zen::ZenEngine>,
}

impl VerificationServiceImpl {
    pub fn new(zen_engine: Arc<zen::ZenEngine>) -> Self {
        Self { zen_engine }
    }
}

#[tonic::async_trait]
impl VerificationService for VerificationServiceImpl {
    async fn evaluate_reconciliation(
        &self,
        request: Request<GrpcReconciliationRequest>,
    ) -> Result<Response<GrpcReconciliationResult>, Status> {
        let req = request.into_inner();

        // Convert to rust_decimal for processing
        let invoice_amt = rust_decimal::Decimal::from_i64(req.invoice_amount_cents)
            .ok_or_else(|| Status::invalid_argument("invoice_amount_cents overflow"))?;
        let bank_amt = rust_decimal::Decimal::from_i64(req.bank_amount_cents)
            .ok_or_else(|| Status::invalid_argument("bank_amount_cents overflow"))?;
        let gl_amt = rust_decimal::Decimal::from_i64(req.gl_amount_cents)
            .ok_or_else(|| Status::invalid_argument("gl_amount_cents overflow"))?;

        // Compute variance (raw decimal math — deterministic, fully tested)
        let variance_dec = decimal_math::compute_variance(
            &[invoice_amt],
            &[bank_amt],
        ).map_err(|_| Status::internal("variance computation failed"))?;

        // Convert variance to cents for Zen Engine evaluation
        let variance_cents = variance_dec.to_i64()
            .ok_or_else(|| Status::internal("variance conversion overflow"))?;

        // Evaluate against tolerance (Zen Engine decision graph)
        let zen_input = ReconciliationInput {
            variance_cents: variance_cents.abs(),
            tolerance_cents: req.tolerance_cents,
        };

        let zen_output = self.zen_engine.evaluate(&zen_input).await
            .map_err(|e| Status::internal(e.to_string()))?;

        // Build human-readable formula string
        let formula = decimal_math::format_formula(
            "GL Reconciliation",
            invoice_amt,
            bank_amt,
            gl_amt,
            variance_dec,
            req.tolerance_cents,
        );

        Ok(Response::new(GrpcReconciliationResult {
            variance_cents: variance_cents,
            exceeds_tolerance: zen_output.exceeds_tolerance,
            calculation_formula: formula,
            rule_id: "gl_reconciliation".to_string(),
            rule_version: self.zen_engine.rule_version.clone(),
            severity: zen_output.severity,
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::zen::ZenEngine;
    use std::sync::Arc;

    #[tokio::test]
    async fn test_reconciliation_exact_match() {
        let zen = Arc::new(ZenEngine::new_for_test());
        let svc = VerificationServiceImpl::new(zen);
        let req = tonic::Request::new(GrpcReconciliationRequest {
            client_book_id: Uuid::new_v4().to_string(),
            invoice_amount_cents: 10000,
            bank_amount_cents: 10000,
            gl_amount_cents: 10000,
            tolerance_cents: 1,
        });
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.variance_cents, 0);
        assert!(!resp.exceeds_tolerance);
        assert_eq!(resp.rule_id, "gl_reconciliation");
        assert_eq!(resp.severity, "info");
    }

    #[tokio::test]
    async fn test_reconciliation_mismatch_flagged() {
        let zen = Arc::new(ZenEngine::new_for_test());
        let svc = VerificationServiceImpl::new(zen);
        let req = tonic::Request::new(GrpcReconciliationRequest {
            client_book_id: Uuid::new_v4().to_string(),
            invoice_amount_cents: 10500,
            bank_amount_cents: 10000,
            gl_amount_cents: 10000,
            tolerance_cents: 1,
        });
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.variance_cents, 500);
        assert!(resp.exceeds_tolerance);
    }
}