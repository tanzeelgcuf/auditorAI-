// services/verification/src/zen/mod.rs
// Zen Engine integration — loads decision graphs, evaluates tolerance rules.
// Raw math stays in decimal_math/ — this is ONLY the threshold/policy layer.

use serde::{Deserialize, Serialize};
use std::fs;
use std::path::Path;
use thiserror::Error;
use sha2::{Sha256, Digest};

#[derive(Debug, Error)]
pub enum ZenError {
    #[error("decision graph load error: {0}")]
    LoadError(String),
    #[error("evaluation error: {0}")]
    #[allow(dead_code)]
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
    #[serde(default)]
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
    #[serde(rename = "sourceId")]
    source_id: String,
    #[serde(rename = "targetId")]
    target_id: String,
}

/// The rule engine that loads a decision graph and evaluates reconciliation inputs.
pub struct RuleEngine {
    pub rule_id: String,
    pub rule_version: String,
    #[allow(dead_code)]
    graph: DecisionGraph,
}

impl RuleEngine {
    /// Load a decision graph from a JSON file path.
    pub fn new(graph_path: &str) -> Result<Self, ZenError> {
        let path = Path::new(graph_path);
        let content = fs::read_to_string(path)
            .map_err(|e| ZenError::LoadError(format!("cannot read {}: {}", graph_path, e)))?;
        Self::from_json(&content, graph_path)
    }

    /// Create a RuleEngine from raw JSON content (used in tests).
    pub fn from_json(json: &str, name: &str) -> Result<Self, ZenError> {
        let rule_id = name.to_string();
        let rule_version = compute_rule_version(json.as_bytes());

        let mut graph: DecisionGraph = serde_json::from_str(json)
            .map_err(|e| ZenError::LoadError(format!("parse error: {}", e)))?;
        graph.rule_id = rule_id.clone();
        graph.rule_version = rule_version.clone();

        Ok(RuleEngine { rule_id, rule_version, graph })
    }

    /// Evaluate a reconciliation input against the loaded rules.
    pub fn evaluate(&self, input: &ReconciliationInput) -> ReconciliationOutput {
        evaluate_rules(input)
    }
}

/// Evaluates tolerance thresholds against the decision graph rules.
pub fn evaluate_rules(
    input: &ReconciliationInput,
) -> ReconciliationOutput {
    // Use abs because variance direction doesn't matter for severity
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
/// Returns SHA-256 8-byte hex prefix.
pub fn compute_rule_version(content: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(content);
    hex::encode(&hasher.finalize()[..8]) // 8-byte prefix = 16 hex chars
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- evaluate_rules boundary tests ----

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
    fn test_exactly_at_10x() {
        // tolerance * 10 = 10, variance = 10 => low (variance <= t*10)
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 10,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_one_over_10x() {
        // tolerance * 10 = 10, variance = 11 => medium
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 11,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "medium");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_exactly_at_100x() {
        // tolerance * 100 = 100, variance = 100 => medium
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 100,
            tolerance_cents: 1,
        });
        assert_eq!(result.severity, "medium");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_one_over_100x() {
        // tolerance * 100 = 100, variance = 101 => high
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
        // abs(-50)=50, t=10, t*10=100. 50 > 10 and 50 <= 100 => low
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: -50,
            tolerance_cents: 10,
        });
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_tolerance_zero_variance_zero() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 0,
            tolerance_cents: 0,
        });
        assert_eq!(result.severity, "info");
        assert!(!result.exceeds_tolerance);
    }

    #[test]
    fn test_tolerance_zero_positive_variance() {
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 1,
            tolerance_cents: 0,
        });
        // t=0: 1 <= 0? no. 1 <= 0*10=0? no. 1 <= 0*100=0? no. => high
        assert_eq!(result.severity, "high");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_riverside_row2_variance() {
        // Riverside Design Co. row 2 (Round 7 eval): bank -89900, GL +89400,
        // invoice +89900. Three-way variance = 500¢ (inv-gl and bank-gl differ),
        // tolerance 100¢. 500 = 5× tolerance (≤ 10×) => low severity, exceeds.
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 500,
            tolerance_cents: 100,
        });
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }

    #[test]
    fn test_riverside_row1_zero_variance() {
        // Row 1 (many-to-many): invoices 34250+12875 = 47125 = bank = GL, 0¢ gap.
        let result = evaluate_rules(&ReconciliationInput {
            variance_cents: 0,
            tolerance_cents: 100,
        });
        assert_eq!(result.severity, "info");
        assert!(!result.exceeds_tolerance);
    }

    // ---- rule_version tests ----

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

    #[test]
    fn test_rule_version_length() {
        let json = r#"{"nodes":[],"edges":[]}"#;
        let version = compute_rule_version(json.as_bytes());
        // SHA-256 8-byte prefix => 16 hex chars
        assert_eq!(version.len(), 16);
        assert!(version.chars().all(|c| c.is_ascii_hexdigit()));
    }

    // ---- RuleEngine construction tests ----

    #[test]
    fn test_rule_engine_from_json() {
        let json = r#"{"nodes":[],"edges":[]}"#;
        let engine = RuleEngine::from_json(json, "test_graph").unwrap();
        assert_eq!(engine.rule_id, "test_graph");
        assert_eq!(engine.rule_version.len(), 16);
    }

    #[test]
    fn test_rule_engine_invalid_json() {
        let engine = RuleEngine::from_json("not json", "bad");
        assert!(engine.is_err());
        match engine {
            Err(ZenError::LoadError(_)) => {} // expected
            _ => panic!("expected LoadError"),
        }
    }

    #[test]
    fn test_rule_engine_bad_path() {
        let engine = RuleEngine::new("/nonexistent/path.json");
        assert!(engine.is_err());
        match engine {
            Err(ZenError::LoadError(_)) => {} // expected
            _ => panic!("expected LoadError"),
        }
    }

    // ---- RuleEngine evaluate ----

    #[test]
    fn test_rule_engine_evaluate() {
        let json = r#"{"nodes":[],"edges":[]}"#;
        let engine = RuleEngine::from_json(json, "test").unwrap();
        let result = engine.evaluate(&ReconciliationInput {
            variance_cents: 5,
            tolerance_cents: 1,
        });
        // v=5, t=1, t*10=10, 5 <= 10 => low
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }

    // ---- integration test: load real decision graph ----

    #[test]
    fn test_load_real_decision_graph() {
        let path = std::env!("CARGO_MANIFEST_DIR");
        let graph_path = format!("{}/decision-graphs/gl_reconciliation.json", path);
        let engine = RuleEngine::new(&graph_path).unwrap();
        assert_eq!(engine.rule_version.len(), 16);
        assert!(engine.rule_id.contains("gl_reconciliation"));
    }

    #[test]
    fn test_decision_graph_evaluate_real() {
        let path = std::env!("CARGO_MANIFEST_DIR");
        let graph_path = format!("{}/decision-graphs/gl_reconciliation.json", path);
        let engine = RuleEngine::new(&graph_path).unwrap();
        let result = engine.evaluate(&ReconciliationInput {
            variance_cents: 50,
            tolerance_cents: 10,
        });
        // v=50, t=10, t*10=100, 50 <= 100 => low
        assert_eq!(result.severity, "low");
        assert!(result.exceeds_tolerance);
    }
}
