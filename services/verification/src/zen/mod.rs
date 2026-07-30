// services/verification/src/zen/mod.rs
// Zen Engine integration — loads decision graphs, evaluates tolerance rules.
// Raw math stays in decimal_math/ — this is ONLY the threshold/policy layer.

use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::Path;
use thiserror::Error;
use sha2::{Sha256, Digest};

#[derive(Debug, Error)]
pub enum ZenError {
    #[error("decision graph load error: {0}")]
    LoadError(String),
    #[error("evaluation error: {0}")]
    EvalError(String),
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReconciliationInput {
    pub variance_cents: i64,
    pub tolerance_cents: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReconciliationOutput {
    pub severity: String,          // info, low, medium, high
    pub exceeds_tolerance: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct DecisionGraph {
    #[serde(skip)]
    pub rule_id: String,
    #[serde(skip)]
    pub rule_version: String,
    pub nodes: Vec<GraphNode>,
    pub edges: Vec<GraphEdge>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct GraphNode {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    name: String,
    content: Option<NodeContent>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct NodeContent {
    rules: Vec<RuleRow>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RuleRow {
    #[serde(rename = "variance_cents")]
    variance_cents: String,
    severity: String,
    exceeds_tolerance: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct GraphEdge {
    id: String,
    source_id: String,
    target_id: String,
}

/// Evaluates tolerance thresholds against the decision graph rules.
pub fn evaluate_rules(
    input: &ReconciliationInput,
) -> ReconciliationOutput {
    // In production, this sends to Zen Engine.
    // For now, implement the same logic directly from the decision graph JSON.
    let v = input.variance_cents.abs();
    let t = input.tolerance_cents;

    if v <= t {
        ReconciliationOutput {
            severity: "info".to_string(),
            exceeds_tolerance: false,
        }
    } else if v <= t * 10 {
        ReconciliationOutput {
            severity: "low".to_string(),
            exceeds_tolerance: true,
        }
    } else if v <= t * 100 {
        ReconciliationOutput {
            severity: "medium".to_string(),
            exceeds_tolerance: true,
        }
    } else {
        ReconciliationOutput {
            severity: "high".to_string(),
            exceeds_tolerance: true,
        }
    }
}

/// Compute rule version hash from decision graph JSON content.
pub fn compute_rule_version(content: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(content);
    hex::encode(&hasher.finalize()[..8]) // 8-byte prefix
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_exact_tolerance() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 1,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "info");
        assert!(!result.exceeds_tolerance);
    }

    #[test]
    fn test_one_cent_over() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 2,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_boundary_low_medium() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 10,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_medium() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 11,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "medium");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_high() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 101,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "high");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_zero_variance() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 0,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "info");
        assert!(!result.exceeds_tolerance);
    }

    #[test]
    fn test_negative_variance_norm() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: -50,
            tolerance_cents: 10,
        });
        assert_eq!(result.severity, "medium");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_rule_version_consistent() {
        let json = r#"{"nodes":[],"edges":[]}"#;
        let v1 = compute_rule_version(json.as_bytes());
        let v2 = compute_rule_version(json.as_bytes());
        assert_eq!(v1, v2, "same content must produce same version");
    }

    #[test]
    fn test_rule_version_changes() {
        let json_a = r#"{"nodes":[{"id":"a"}],"edges":[]}"#;
        let json_b = r#"{"nodes":[{"id":"b"}],"edges":[]}"#;
        let va = compute_rule_version(json_a.as_bytes());
        let vb = compute_rule_version(json_b.as_bytes());
        assert_ne!(va, vb, "different content must produce different version");
    }
}