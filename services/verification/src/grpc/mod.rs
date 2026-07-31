// services/verification/src/grpc/mod.rs
use tonic::{Request, Response, Status};
use uuid::Uuid;
use std::sync::Arc;

use rust_decimal::prelude::*;

use crate::decimal_math;
use crate::zen::{RuleEngine, ReconciliationInput};

#[allow(clippy::unwrap_used)]
pub mod verification_service {
    tonic::include_proto!("verification");
}

use verification_service::{
    verification_service_server::VerificationService,
    ReconciliationRequest as GrpcReconciliationRequest,
    ReconciliationResult as GrpcReconciliationResult,
    BatchReconciliationRequest as GrpcBatchReconciliationRequest,
    BatchReconciliationResult as GrpcBatchReconciliationResult,
    GroupResult as GrpcGroupResult,
};

pub struct VerificationServiceImpl {
    rule_engine: Arc<RuleEngine>,
}

impl VerificationServiceImpl {
    pub fn new(rule_engine: Arc<RuleEngine>) -> Self {
        Self { rule_engine }
    }

    fn evaluate_single_group(
        &self,
        inv_total_cents: i64,
        bank_total_cents: i64,
        gl_total_cents: i64,
        tolerance_cents: i32,
        group_id: &str,
    ) -> Result<GrpcGroupResult, Status> {
        // Convert to Decimal for arithmetic
        let inv_dec = rust_decimal::Decimal::from_i64(inv_total_cents)
            .ok_or_else(|| Status::invalid_argument("inv_total_cents overflow"))?;
        let bank_dec = rust_decimal::Decimal::from_i64(bank_total_cents)
            .ok_or_else(|| Status::invalid_argument("bank_total_cents overflow"))?;
        let gl_dec = rust_decimal::Decimal::from_i64(gl_total_cents)
            .ok_or_else(|| Status::invalid_argument("gl_total_cents overflow"))?;

        // Compute three-way variance
        let (vib, v_igl, v_bgl) = decimal_math::compute_three_way_variance(
            &[inv_dec],
            &[bank_dec],
            &[gl_dec],
        ).map_err(|e| Status::internal(format!("variance computation failed: {}", e)))?;

        // Find max variance across the 3 comparisons
        let max_variance = vib.max(v_igl).max(v_bgl);

        // Convert max variance to cents for severity evaluation
        let variance_cents = max_variance.to_i64()
            .ok_or_else(|| Status::internal("variance conversion overflow"))?;

        // Evaluate against tolerance
        let zen_input = ReconciliationInput {
            variance_cents,
            tolerance_cents: tolerance_cents as i64,
        };
        let zen_output = self.rule_engine.evaluate(&zen_input);

        // Build formula string
        let formula = decimal_math::format_formula(
            &format!("Group {}", group_id),
            inv_dec,
            bank_dec,
            gl_dec,
            max_variance,
            tolerance_cents as i64,
        );

        Ok(GrpcGroupResult {
            group_id: group_id.to_string(),
            variance_cents,
            exceeds_tolerance: zen_output.exceeds_tolerance,
            calculation_formula: formula,
            rule_id: self.rule_engine.rule_id.clone(),
            rule_version: self.rule_engine.rule_version.clone(),
            severity: zen_output.severity,
        })
    }
}

#[tonic::async_trait]
impl VerificationService for VerificationServiceImpl {
    async fn evaluate_reconciliation(
        &self,
        request: Request<GrpcReconciliationRequest>,
    ) -> Result<Response<GrpcReconciliationResult>, Status> {
        let req = request.into_inner();

        // Validate UUID
        let _client_book_id = Uuid::parse_str(&req.client_book_id)
            .map_err(|e| Status::invalid_argument(format!("invalid client_book_id: {}", e)))?;

        // Convert to rust_decimal for processing
        let invoice_amt = rust_decimal::Decimal::from_i64(req.invoice_amount_cents)
            .ok_or_else(|| Status::invalid_argument("invoice_amount_cents overflow"))?;
        let bank_amt = rust_decimal::Decimal::from_i64(req.bank_amount_cents)
            .ok_or_else(|| Status::invalid_argument("bank_amount_cents overflow"))?;
        let gl_amt = rust_decimal::Decimal::from_i64(req.gl_amount_cents)
            .ok_or_else(|| Status::invalid_argument("gl_amount_cents overflow"))?;

        // Compute three-way variance (gives us all pairwise variances at once)
        let (vib, v_igl, v_bgl) = decimal_math::compute_three_way_variance(
            &[invoice_amt],
            &[bank_amt],
            &[gl_amt],
        ).map_err(|e| Status::internal(format!("variance computation failed: {}", e)))?;

        // The max variance across the three comparisons
        let max_variance = vib.max(v_igl).max(v_bgl);

        // Convert to cents for severity evaluation
        let variance_cents = max_variance.to_i64()
            .ok_or_else(|| Status::internal("variance conversion overflow"))?;

        // Evaluate against tolerance (zen engine decision graph)
        let zen_input = ReconciliationInput {
            variance_cents,
            tolerance_cents: req.tolerance_cents as i64,
        };
        let zen_output = self.rule_engine.evaluate(&zen_input);

        // Build human-readable formula string
        let formula = decimal_math::format_formula(
            "GL Reconciliation",
            invoice_amt,
            bank_amt,
            gl_amt,
            max_variance,
            req.tolerance_cents as i64,
        );

        Ok(Response::new(GrpcReconciliationResult {
            variance_cents,
            exceeds_tolerance: zen_output.exceeds_tolerance,
            calculation_formula: formula,
            rule_id: self.rule_engine.rule_id.clone(),
            rule_version: self.rule_engine.rule_version.clone(),
            severity: zen_output.severity,
        }))
    }

    async fn batch_evaluate(
        &self,
        request: Request<GrpcBatchReconciliationRequest>,
    ) -> Result<Response<GrpcBatchReconciliationResult>, Status> {
        let req = request.into_inner();

        // Validate UUID
        let _client_book_id = Uuid::parse_str(&req.client_book_id)
            .map_err(|e| Status::invalid_argument(format!("invalid client_book_id: {}", e)))?;

        let mut results: Vec<GrpcGroupResult> = Vec::with_capacity(req.groups.len());

        for group in req.groups {
            let result = self.evaluate_single_group(
                group.invoice_total_cents,
                group.bank_total_cents,
                group.gl_total_cents,
                group.tolerance_cents,
                &group.group_id,
            )?;
            results.push(result);
        }

        Ok(Response::new(GrpcBatchReconciliationResult { results }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use verification_service::GroupReconciliation as GrpcGroupReconciliation;

    // Helper: create a test RuleEngine from inline JSON
    fn test_engine() -> Arc<RuleEngine> {
        let json = r#"{"nodes":[],"edges":[]}"#;
        Arc::new(RuleEngine::from_json(json, "test_rule").unwrap())
    }

    fn make_req(
        client_book_id: &str,
        invoice: i64,
        bank: i64,
        gl: i64,
        tolerance: i32,
    ) -> GrpcReconciliationRequest {
        GrpcReconciliationRequest {
            client_book_id: client_book_id.to_string(),
            invoice_amount_cents: invoice,
            bank_amount_cents: bank,
            gl_amount_cents: gl,
            tolerance_cents: tolerance,
        }
    }

    fn make_group(
        group_id: &str,
        invoice: i64,
        bank: i64,
        gl: i64,
        tolerance: i32,
    ) -> GrpcGroupReconciliation {
        GrpcGroupReconciliation {
            group_id: group_id.to_string(),
            invoice_total_cents: invoice,
            bank_total_cents: bank,
            gl_total_cents: gl,
            tolerance_cents: tolerance,
        }
    }

    // ---- single evaluation tests ----

    #[tokio::test]
    async fn test_reconciliation_exact_match() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10000, 10000, 10000, 1,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.variance_cents, 0);
        assert!(!resp.exceeds_tolerance);
        assert_eq!(resp.rule_id, "test_rule");
        assert_eq!(resp.severity, "info");
        assert_eq!(resp.rule_version.len(), 16);
        assert!(resp.calculation_formula.contains("variance=0.00"));
    }

    #[tokio::test]
    async fn test_reconciliation_small_variance() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        // variance = 1 cent, tolerance = 10 => info (within tolerance)
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10001, 10000, 10000, 10,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.variance_cents, 1);
        assert!(!resp.exceeds_tolerance);
        assert_eq!(resp.severity, "info");
    }

    #[tokio::test]
    async fn test_reconciliation_low() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        // variance = 2 cents, tolerance = 1 => low
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10002, 10000, 10000, 1,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.severity, "low");
        assert!(resp.exceeds_tolerance);
    }

    #[tokio::test]
    async fn test_reconciliation_medium() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        // variance = 15 cents, tolerance = 1 => medium (10 < 15 <= 100)
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10015, 10000, 10000, 1,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.severity, "medium");
        assert!(resp.exceeds_tolerance);
    }

    #[tokio::test]
    async fn test_reconciliation_high() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        // variance = 150 cents, tolerance = 1 => high (> 100)
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10150, 10000, 10000, 1,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.severity, "high");
        assert!(resp.exceeds_tolerance);
    }

    #[tokio::test]
    async fn test_reconciliation_bank_gl_mismatch() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        // invoice=10000, bank=10000, gl=10050 => variance = 50 (igl)
        // t=1, t*10=10, t*100=100. 50 > 10 and 50 <= 100 => medium
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10000, 10000, 10050, 1,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.variance_cents, 50);
        assert_eq!(resp.severity, "medium");
    }

    #[tokio::test]
    async fn test_reconciliation_invalid_uuid() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        let req = tonic::Request::new(make_req(
            "not-a-uuid",
            10000, 10000, 10000, 1,
        ));
        let result = svc.evaluate_reconciliation(req).await;
        assert!(result.is_err());
        assert_eq!(result.unwrap_err().code(), tonic::Code::InvalidArgument);
    }

    #[tokio::test]
    async fn test_reconciliation_three_way_variance_used() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        // All three differ: inv=100, bank=95, gl=90
        // vib=5, igl=10, bgl=5 => max = 10
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            100, 95, 90, 1,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.variance_cents, 10);
    }

    // ---- batch evaluation tests ----

    #[tokio::test]
    async fn test_batch_evaluate_empty() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        let req = tonic::Request::new(GrpcBatchReconciliationRequest {
            client_book_id: Uuid::new_v4().to_string(),
            groups: vec![],
        });
        let resp = svc.batch_evaluate(req).await.unwrap().into_inner();
        assert!(resp.results.is_empty());
    }

    #[tokio::test]
    async fn test_batch_evaluate_one_group() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        let req = tonic::Request::new(GrpcBatchReconciliationRequest {
            client_book_id: Uuid::new_v4().to_string(),
            groups: vec![make_group("g1", 10000, 10000, 10000, 1)],
        });
        let resp = svc.batch_evaluate(req).await.unwrap().into_inner();
        assert_eq!(resp.results.len(), 1);
        assert_eq!(resp.results[0].group_id, "g1");
        assert_eq!(resp.results[0].variance_cents, 0);
        assert_eq!(resp.results[0].severity, "info");
    }

    #[tokio::test]
    async fn test_batch_evaluate_multiple_groups() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        let req = tonic::Request::new(GrpcBatchReconciliationRequest {
            client_book_id: Uuid::new_v4().to_string(),
            groups: vec![
                // group 1: exact match
                make_group("g1", 10000, 10000, 10000, 1),
                // group 2: small variance
                make_group("g2", 10002, 10000, 10000, 1),
                // group 3: large variance
                make_group("g3", 20000, 10000, 10000, 1),
            ],
        });
        let resp = svc.batch_evaluate(req).await.unwrap().into_inner();
        assert_eq!(resp.results.len(), 3);

        assert_eq!(resp.results[0].group_id, "g1");
        assert_eq!(resp.results[0].variance_cents, 0);
        assert_eq!(resp.results[0].severity, "info");
        assert!(!resp.results[0].exceeds_tolerance);

        assert_eq!(resp.results[1].group_id, "g2");
        assert_eq!(resp.results[1].variance_cents, 2);
        assert_eq!(resp.results[1].severity, "low");
        assert!(resp.results[1].exceeds_tolerance);

        assert_eq!(resp.results[2].group_id, "g3");
        assert_eq!(resp.results[2].variance_cents, 10000);
        assert_eq!(resp.results[2].severity, "high");
        assert!(resp.results[2].exceeds_tolerance);
    }

    #[tokio::test]
    async fn test_batch_evaluate_invalid_uuid() {
        let engine = test_engine();
        let svc = VerificationServiceImpl::new(engine);
        let req = tonic::Request::new(GrpcBatchReconciliationRequest {
            client_book_id: "bad-uuid".to_string(),
            groups: vec![make_group("g1", 10000, 10000, 10000, 1)],
        });
        let result = svc.batch_evaluate(req).await;
        assert!(result.is_err());
        assert_eq!(result.unwrap_err().code(), tonic::Code::InvalidArgument);
    }

    // ---- integration test: decision graph at runtime ----

    #[tokio::test]
    async fn test_runtime_decision_graph_changes_output() {
        // Load the real decision graph from disk
        let manifest_dir = std::env!("CARGO_MANIFEST_DIR");
        let graph_path = format!("{}/decision-graphs/gl_reconciliation.json", manifest_dir);
        let engine = Arc::new(RuleEngine::new(&graph_path).unwrap());
        let svc = VerificationServiceImpl::new(engine);

        // Same input but with different tolerance (passed at runtime, not compile-time)
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10050, 10000, 10000, 10,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        // variance=50, tolerance=10 => low (10<50<=100)
        assert_eq!(resp.severity, "low");

        // With tolerance=100, variance=50 => info (within tolerance)
        let req = tonic::Request::new(make_req(
            &Uuid::new_v4().to_string(),
            10050, 10000, 10000, 100,
        ));
        let resp = svc.evaluate_reconciliation(req).await.unwrap().into_inner();
        assert_eq!(resp.severity, "info");
        assert!(!resp.exceeds_tolerance);
    }
}
