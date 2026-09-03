// services/verification/src/zen/mod.rs
//
// Decision-graph rule evaluation for reconciliation tolerance policy.
//
// WHAT THIS IS: a compiler and evaluator for the decision-table subset of the
// Zen (gorules) graph JSON format. It reads decision-graphs/*.json, compiles
// every rule row's range expression into a typed form at LOAD time, and
// evaluates a variance against those compiled rows at request time.
//
// WHAT THIS IS NOT: the `zen-engine` crate. services/verification/Cargo.toml has
// no zen dependency (verified: `grep -n zen Cargo.toml` -> no match). The graph
// FORMAT is Zen-compatible so the files stay portable, but evaluation is this
// module. Repo docs that say "built on Zen Engine" overstate that; the accurate
// statement is "Zen-format decision graphs, evaluated in-crate".
//
// THE BUG THIS FILE WAS REWRITTEN TO FIX (found 2026-09-04):
//   `RuleEngine::evaluate` called a free function `evaluate_rules(input)` and
//   did NOT pass `self.graph`. The graph field was marked #[allow(dead_code)] —
//   the compiler had already noticed nothing read it. Severity came from a
//   hardcoded ladder (v<=t -> info, <=t*10 -> low, <=t*100 -> medium, else
//   high) that happened to coincide with gl_reconciliation.json.
//
//   That coincidence is why it survived: every test passed, and outputs matched
//   the graph — as long as nobody edited the graph. `rule_version` (a SHA-256
//   prefix of the graph JSON) IS recorded on every gRPC result
//   (grpc/mod.rs:101,175), so the audit trail asserted "this severity was
//   produced by rule_version=<hash>" about a file that had no effect on it.
//   Change a firm's tolerance ladder, redeploy, see a new rule_version in the
//   audit trail, and get identical severities from the old hardcoded ladder.
//   That directly falsifies non-negotiable #3 in CLAUDE.md: every figure must
//   carry "the exact rule/calculation that produced it".
//
//   The proof was already committed and had been read as a passing test:
//   `test_rule_engine_evaluate` built an engine from {"nodes":[],"edges":[]} —
//   a graph with ZERO rules — and asserted severity == "low". A graph with no
//   rules can only yield a confident severity if the graph is ignored.
//
// DESIGN CONSEQUENCE: there is now exactly ONE source of tolerance policy, the
// graph. The hardcoded ladder is deleted rather than kept as a fallback,
// because a fallback is what made the original defect invisible. A graph that
// cannot be fully compiled fails to LOAD, so main.rs:44 refuses to start the
// service and logs why, instead of serving decisions attributed to a file it
// could not interpret.
//
// Raw money math stays in decimal_math/. This module only maps an
// already-computed variance onto a severity band.

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fs;
use std::path::Path;
use thiserror::Error;

/// The only identifier a range expression may reference. Anything else fails to
/// compile rather than silently evaluating to zero.
const TOLERANCE_IDENT: &str = "tolerance_cents";

/// Must match `infra/init.sql:173`:
/// `severity TEXT NOT NULL CHECK (severity IN ('info','low','medium','high'))`.
/// Validated at load so a typo in a graph is a startup failure with the row
/// quoted, not a constraint violation on INSERT three services downstream.
const VALID_SEVERITIES: [&str; 4] = ["info", "low", "medium", "high"];

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
    pub severity: String,
    pub exceeds_tolerance: bool,
}

// ---------------------------------------------------------------------------
// Wire format. Mirrors the Zen graph JSON. Unknown fields (a node's `inputs`
// and `outputs`, editor metadata) are ignored by serde, which is what keeps
// these files editable in Zen tooling without breaking this loader.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize)]
struct DecisionGraph {
    nodes: Vec<GraphNode>,
    // `edges` is deliberately NOT modelled. serde ignores unknown keys, so the
    // JSON keeps its edges and Zen tooling keeps working; this evaluator resolves
    // the decision table by node type, so an edge struct would be data nothing
    // evaluates.
    //
    // Related, and stated narrowly because it is what I verified: this module
    // carries no #[allow(dead_code)] anywhere. The version it replaces used that
    // annotation on the `graph` field (old line 77) and on `ZenError::EvalError`
    // (old line 15) — and the unread `graph` field WAS the bug. A dead_code
    // warning in this module is the compiler correctly reporting that the
    // evaluator ignores something, and must be fixed by wiring it, not silenced.
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct GraphNode {
    id: String,
    #[serde(rename = "type")]
    node_type: String,
    #[serde(default)]
    name: String,
    #[serde(default)]
    content: Option<NodeContent>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct NodeContent {
    #[serde(default)]
    rules: Vec<RuleRow>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct RuleRow {
    variance_cents: String,
    severity: String,
    exceeds_tolerance: String,
}

// ---------------------------------------------------------------------------
// Compiled form. Every range expression in the graph is turned into this at
// LOAD time, so a malformed graph is a startup error rather than a per-request
// surprise, and evaluation does no string work.
// ---------------------------------------------------------------------------

/// A bound operand in canonical linear form: `tol_coeff * tolerance_cents + konst`.
///
/// The whole grammar the graph may use, deliberately tiny:
///   `0`                      -> { tol_coeff: 0,   konst: 0 }
///   `250`                    -> { tol_coeff: 0,   konst: 250 }
///   `tolerance_cents`        -> { tol_coeff: 1,   konst: 0 }
///   `tolerance_cents*10`     -> { tol_coeff: 10,  konst: 0 }
///   `100*tolerance_cents`    -> { tol_coeff: 100, konst: 0 }
///
/// Anything else fails to compile. This is a deliberate refusal to grow an
/// expression language: a rule table nobody can fully evaluate is how the
/// original defect (a graph that was parsed but never consulted) stayed
/// invisible. If a firm needs richer policy than this grammar expresses, that
/// belongs in a new node type with its own tests, not in a permissive parser.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Operand {
    tol_coeff: i64,
    konst: i64,
}

impl Operand {
    fn parse(raw: &str) -> Result<Self, ZenError> {
        let s: String = raw.chars().filter(|c| !c.is_whitespace()).collect();
        if s.is_empty() {
            return Err(ZenError::LoadError("empty bound operand".to_string()));
        }
        let factors: Vec<&str> = s.split('*').collect();
        match factors.len() {
            1 => Self::atom(factors[0]),
            2 => {
                let (a, b) = (factors[0], factors[1]);
                let (ta, tb) = (Self::atom(a)?, Self::atom(b)?);
                // Exactly one side may be the identifier; tolerance_cents squared
                // is not a linear form and is rejected rather than approximated.
                match (ta.tol_coeff, tb.tol_coeff) {
                    (0, 0) => {
                        let k = ta.konst.checked_mul(tb.konst).ok_or_else(|| {
                            ZenError::LoadError(format!("constant overflow in '{}'", raw))
                        })?;
                        Ok(Operand { tol_coeff: 0, konst: k })
                    }
                    (1, 0) => Ok(Operand { tol_coeff: tb.konst, konst: 0 }),
                    (0, 1) => Ok(Operand { tol_coeff: ta.konst, konst: 0 }),
                    _ => Err(ZenError::LoadError(format!(
                        "'{}' is not linear in {} (at most one {} factor)",
                        raw, TOLERANCE_IDENT, TOLERANCE_IDENT
                    ))),
                }
            }
            _ => Err(ZenError::LoadError(format!(
                "'{}' has too many factors; grammar is N, {ident}, or {ident}*N",
                raw,
                ident = TOLERANCE_IDENT
            ))),
        }
    }

    fn atom(tok: &str) -> Result<Self, ZenError> {
        if tok == TOLERANCE_IDENT {
            return Ok(Operand { tol_coeff: 1, konst: 0 });
        }
        tok.parse::<i64>()
            .map(|n| Operand { tol_coeff: 0, konst: n })
            .map_err(|_| {
                ZenError::LoadError(format!(
                    "'{}' is neither an integer nor the identifier {}",
                    tok, TOLERANCE_IDENT
                ))
            })
    }

    /// Resolve against a concrete tolerance. Checked arithmetic: a tolerance
    /// large enough to overflow `tol_coeff * t` must surface as an error, not
    /// wrap into a negative threshold that silently reclassifies every variance.
    fn eval(self, tolerance_cents: i64) -> Result<i64, ZenError> {
        self.tol_coeff
            .checked_mul(tolerance_cents)
            .and_then(|p| p.checked_add(self.konst))
            .ok_or_else(|| {
                ZenError::EvalError(format!(
                    "bound {}*{} + {} overflows i64",
                    self.tol_coeff, tolerance_cents, self.konst
                ))
            })
    }
}

#[derive(Debug, Clone, Copy)]
enum Bound {
    Unbounded,
    Inclusive(Operand),
    Exclusive(Operand),
}

impl Bound {
    fn operand(self) -> Option<Operand> {
        match self {
            Bound::Unbounded => None,
            Bound::Inclusive(o) | Bound::Exclusive(o) => Some(o),
        }
    }

    fn is_inclusive(self) -> bool {
        matches!(self, Bound::Inclusive(_))
    }
}

#[derive(Debug, Clone)]
struct CompiledRule {
    lo: Bound,
    hi: Bound,
    severity: String,
    exceeds_tolerance: bool,
    /// The original expression text, kept for error messages and for
    /// `describe()` so a mis-set band can be read off a log line.
    source: String,
}

impl CompiledRule {
    fn matches(&self, variance: i64, tolerance: i64) -> Result<bool, ZenError> {
        let lo_ok = match self.lo {
            Bound::Unbounded => true,
            Bound::Inclusive(o) => variance >= o.eval(tolerance)?,
            Bound::Exclusive(o) => variance > o.eval(tolerance)?,
        };
        if !lo_ok {
            return Ok(false);
        }
        let hi_ok = match self.hi {
            Bound::Unbounded => true,
            Bound::Inclusive(o) => variance <= o.eval(tolerance)?,
            Bound::Exclusive(o) => variance < o.eval(tolerance)?,
        };
        Ok(hi_ok)
    }
}

// ---------------------------------------------------------------------------
// Compilation and validation
// ---------------------------------------------------------------------------

/// Parse a Zen decision-table range literal into a pair of bounds.
///
/// Accepted: `[0..tolerance_cents]`, `(tolerance_cents..tolerance_cents*10]`,
/// `(tolerance_cents*100..]`, `[250..1000)`. An omitted side is unbounded.
/// Splitting on `..` is unambiguous because the operand grammar has no decimal
/// point — a fractional threshold fails `parse::<i64>` with the token quoted.
fn parse_range(expr: &str) -> Result<(Bound, Bound), ZenError> {
    let t = expr.trim();
    if !t.is_ascii() {
        return Err(ZenError::LoadError(format!(
            "range '{}' contains non-ASCII characters",
            expr
        )));
    }
    if t.len() < 3 {
        return Err(ZenError::LoadError(format!(
            "range '{}' is too short to be a bracketed interval",
            expr
        )));
    }
    let bytes = t.as_bytes();
    let open = bytes[0];
    let close = bytes[t.len() - 1];
    if open != b'[' && open != b'(' {
        return Err(ZenError::LoadError(format!(
            "range '{}' must open with '[' or '('",
            expr
        )));
    }
    if close != b']' && close != b')' {
        return Err(ZenError::LoadError(format!(
            "range '{}' must close with ']' or ')'",
            expr
        )));
    }
    let interior = &t[1..t.len() - 1];
    let (lo_s, hi_s) = interior.split_once("..").ok_or_else(|| {
        ZenError::LoadError(format!("range '{}' has no '..' separator", expr))
    })?;

    let lo = if lo_s.trim().is_empty() {
        Bound::Unbounded
    } else {
        let o = Operand::parse(lo_s)?;
        if open == b'[' {
            Bound::Inclusive(o)
        } else {
            Bound::Exclusive(o)
        }
    };
    let hi = if hi_s.trim().is_empty() {
        Bound::Unbounded
    } else {
        let o = Operand::parse(hi_s)?;
        if close == b']' {
            Bound::Inclusive(o)
        } else {
            Bound::Exclusive(o)
        }
    };
    Ok((lo, hi))
}

/// Compile the graph's single decision table into ordered, typed bands.
fn compile(graph: &DecisionGraph) -> Result<Vec<CompiledRule>, ZenError> {
    let tables: Vec<&GraphNode> = graph
        .nodes
        .iter()
        .filter(|n| n.node_type == "decisionTableNode")
        .collect();
    if tables.is_empty() {
        return Err(ZenError::LoadError(
            "graph declares no decisionTableNode, so it expresses no tolerance \
             policy. A graph that cannot produce a severity must not load: the \
             defect this check exists for was an engine that accepted \
             {\"nodes\":[],\"edges\":[]} and still answered \"low\"."
                .to_string(),
        ));
    }
    if tables.len() > 1 {
        let ids: Vec<String> = tables
            .iter()
            .map(|n| format!("{} ('{}')", n.id, n.name))
            .collect();
        return Err(ZenError::LoadError(format!(
            "graph declares {} decisionTableNodes ({}); exactly one is \
             supported, because with two the recorded rule_version would not \
             identify which table produced a severity",
            tables.len(),
            ids.join(", ")
        )));
    }
    let table = tables[0];
    let content = table.content.as_ref().ok_or_else(|| {
        ZenError::LoadError(format!(
            "decisionTableNode {} ('{}') has no content",
            table.id, table.name
        ))
    })?;
    if content.rules.is_empty() {
        return Err(ZenError::LoadError(format!(
            "decisionTableNode {} ('{}') has zero rules",
            table.id, table.name
        )));
    }

    let mut compiled = Vec::with_capacity(content.rules.len());
    for (i, row) in content.rules.iter().enumerate() {
        let (lo, hi) = parse_range(&row.variance_cents)
            .map_err(|e| ZenError::LoadError(format!("rule row {}: {}", i, e)))?;
        // Variance reaches evaluation as |v|, so a negative threshold can never
        // be crossed and silently makes its band dead.
        for b in [lo, hi] {
            if let Some(o) = b.operand() {
                if o.tol_coeff < 0 || o.konst < 0 {
                    return Err(ZenError::LoadError(format!(
                        "rule row {}: '{}' has a negative bound; variance is \
                         compared as an absolute value, so the band is \
                         unreachable",
                        i, row.variance_cents
                    )));
                }
            }
        }
        if !VALID_SEVERITIES.contains(&row.severity.as_str()) {
            return Err(ZenError::LoadError(format!(
                "rule row {}: severity '{}' is not one of {:?} (infra/init.sql \
                 has CHECK (severity IN (...)), so this would fail on INSERT \
                 two services downstream instead of here)",
                i, row.severity, VALID_SEVERITIES
            )));
        }
        let exceeds = match row.exceeds_tolerance.trim() {
            "true" => true,
            "false" => false,
            other => {
                return Err(ZenError::LoadError(format!(
                    "rule row {}: exceeds_tolerance '{}' must be \"true\" or \
                     \"false\"",
                    i, other
                )))
            }
        };
        compiled.push(CompiledRule {
            lo,
            hi,
            severity: row.severity.clone(),
            exceeds_tolerance: exceeds,
            source: row.variance_cents.clone(),
        });
    }

    validate_coverage(&compiled)?;
    Ok(compiled)
}

/// Require the bands to tile `[0..infinity)` exactly once: start at inclusive 0,
/// end unbounded, and meet edge-to-edge with exactly one side inclusive.
///
/// This is what lets `evaluate` be a first-match walk with no fallback band. The
/// original code had a fallback (an `else` arm returning "high"), and a fallback
/// is precisely what let a graph with no rules at all still return an answer.
fn validate_coverage(rules: &[CompiledRule]) -> Result<(), ZenError> {
    let zero = Operand { tol_coeff: 0, konst: 0 };
    let first = rules
        .first()
        .ok_or_else(|| ZenError::LoadError("no rules to validate".to_string()))?;
    if first.lo.operand() != Some(zero) || !first.lo.is_inclusive() {
        return Err(ZenError::LoadError(format!(
            "first band '{}' must start at inclusive 0; variance is compared as \
             |v| so 0 is in the domain and must be covered",
            first.source
        )));
    }
    let last = rules
        .last()
        .ok_or_else(|| ZenError::LoadError("no rules to validate".to_string()))?;
    if !matches!(last.hi, Bound::Unbounded) {
        return Err(ZenError::LoadError(format!(
            "last band '{}' must be unbounded above, or a large variance would \
             match no band and have no severity",
            last.source
        )));
    }
    for pair in rules.windows(2) {
        if let [a, b] = pair {
            let ah = a.hi.operand().ok_or_else(|| {
                ZenError::LoadError(format!(
                    "band '{}' is unbounded above but is not the last band",
                    a.source
                ))
            })?;
            let bl = b.lo.operand().ok_or_else(|| {
                ZenError::LoadError(format!(
                    "band '{}' is unbounded below but is not the first band",
                    b.source
                ))
            })?;
            if ah != bl {
                return Err(ZenError::LoadError(format!(
                    "bands '{}' and '{}' do not meet: consecutive bands must \
                     share a bound, or variances in the gap get no severity",
                    a.source, b.source
                )));
            }
            if a.hi.is_inclusive() == b.lo.is_inclusive() {
                return Err(ZenError::LoadError(format!(
                    "bands '{}' and '{}' meet at the same value and {} it; \
                     exactly one side must be inclusive",
                    a.source,
                    b.source,
                    if a.hi.is_inclusive() {
                        "both include"
                    } else {
                        "neither includes"
                    }
                )));
            }
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// RuleEngine
// ---------------------------------------------------------------------------

pub struct RuleEngine {
    /// Recorded on every gRPC result alongside `rule_version`. Currently the
    /// graph FILE PATH (main.rs passes --decision-graph-path straight through),
    /// which is a known weakness tracked separately: a path is not a stable
    /// rule identity across deployments.
    pub rule_id: String,
    /// SHA-256 prefix of the graph JSON. Now an honest provenance claim: the
    /// bands compiled from that exact byte sequence are the bands that produced
    /// the severity. Before this rewrite it identified a file that had no effect
    /// on the result.
    pub rule_version: String,
    rules: Vec<CompiledRule>,
}

impl RuleEngine {
    /// Load and compile a decision graph from a JSON file path.
    pub fn new(graph_path: &str) -> Result<Self, ZenError> {
        let path = Path::new(graph_path);
        let content = fs::read_to_string(path)
            .map_err(|e| ZenError::LoadError(format!("cannot read {}: {}", graph_path, e)))?;
        Self::from_json(&content, graph_path)
    }

    /// Compile a decision graph from raw JSON.
    ///
    /// Fails on any graph this module cannot fully evaluate. That strictness is
    /// the fix: `main.rs` propagates the error and the service does not start,
    /// which is strictly better than starting and attributing hardcoded
    /// severities to a graph it could not read.
    pub fn from_json(json: &str, name: &str) -> Result<Self, ZenError> {
        let rule_version = compute_rule_version(json.as_bytes());
        let graph: DecisionGraph = serde_json::from_str(json)
            .map_err(|e| ZenError::LoadError(format!("parse error: {}", e)))?;
        let rules = compile(&graph)?;
        Ok(RuleEngine {
            rule_id: name.to_string(),
            rule_version,
            rules,
        })
    }

    /// Evaluate a variance against the compiled bands. First match wins, which
    /// mirrors a Zen decision table's single-hit policy.
    ///
    /// Returns `Result` rather than an infallible `ReconciliationOutput` on
    /// purpose. The alternatives were a panic (banned outside tests) or a
    /// default band — and a default band is the shape of the original bug. A
    /// caller that cannot get a graph-derived severity must fail the request,
    /// not record an invented one against this `rule_version`.
    pub fn evaluate(&self, input: &ReconciliationInput) -> Result<ReconciliationOutput, ZenError> {
        // Direction does not affect severity: a 500¢ shortfall and a 500¢
        // overage are the same size of exception.
        let variance = input.variance_cents.checked_abs().ok_or_else(|| {
            ZenError::EvalError("variance i64::MIN has no absolute value".to_string())
        })?;
        for rule in &self.rules {
            if rule.matches(variance, input.tolerance_cents)? {
                return Ok(ReconciliationOutput {
                    severity: rule.severity.clone(),
                    exceeds_tolerance: rule.exceeds_tolerance,
                });
            }
        }
        // validate_coverage proved the bands tile [0..inf), so reaching here
        // means a defect in THIS module, not in the operator's graph. Say so.
        Err(ZenError::EvalError(format!(
            "no band matched |variance|={} at tolerance={} in rule_id={} \
             rule_version={} — the graph passed coverage validation, so this is \
             an evaluator bug, not a graph error. Bands: {}",
            variance,
            input.tolerance_cents,
            self.rule_id,
            self.rule_version,
            self.describe()
        )))
    }

    /// Number of compiled bands. Lets a test assert the table was actually
    /// compiled, rather than only that loading returned Ok.
    pub fn rule_count(&self) -> usize {
        self.rules.len()
    }

    /// The bands in force, as text. Logged at startup by main.rs so an operator
    /// can read the applied policy off the boot log instead of inferring it.
    pub fn describe(&self) -> String {
        self.rules
            .iter()
            .map(|r| {
                format!(
                    "{} => {}{}",
                    r.source,
                    r.severity,
                    if r.exceeds_tolerance { "/exceeds" } else { "" }
                )
            })
            .collect::<Vec<_>>()
            .join(", ")
    }
}

/// Compute the rule version hash from decision graph JSON content.
/// SHA-256, 8-byte prefix, 16 hex chars.
pub fn compute_rule_version(content: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(content);
    hex::encode(&hasher.finalize()[..8])
}

#[cfg(test)]
mod tests {
    use super::*;

    // ---- helpers ----------------------------------------------------------

    fn row(range: &str, severity: &str, exceeds: bool) -> String {
        format!(
            r#"{{"variance_cents":"{}","severity":"{}","exceeds_tolerance":"{}"}}"#,
            range, severity, exceeds
        )
    }

    fn graph_with(rules: &[String]) -> String {
        format!(
            r#"{{"nodes":[
                {{"id":"in","type":"inputNode","name":"request"}},
                {{"id":"t","type":"decisionTableNode","name":"Tolerance Evaluation",
                  "content":{{"rules":[{}]}}}},
                {{"id":"out","type":"outputNode","name":"result"}}
            ],"edges":[]}}"#,
            rules.join(",")
        )
    }

    /// The same four bands the shipped decision-graphs/gl_reconciliation.json
    /// declares. Kept in the test file so a graph edit cannot silently rewrite
    /// what these tests believe they are asserting.
    fn standard_rules() -> Vec<String> {
        vec![
            row("[0..tolerance_cents]", "info", false),
            row("(tolerance_cents..tolerance_cents*10]", "low", true),
            row("(tolerance_cents*10..tolerance_cents*100]", "medium", true),
            row("(tolerance_cents*100..]", "high", true),
        ]
    }

    fn standard_engine() -> RuleEngine {
        RuleEngine::from_json(&graph_with(&standard_rules()), "test_standard")
            .expect("standard four-band graph must compile")
    }

    fn eval(engine: &RuleEngine, variance: i64, tolerance: i64) -> ReconciliationOutput {
        engine
            .evaluate(&ReconciliationInput {
                variance_cents: variance,
                tolerance_cents: tolerance,
            })
            .expect("standard graph must produce a band for every variance")
    }

    fn real_graph_path() -> String {
        format!(
            "{}/decision-graphs/gl_reconciliation.json",
            std::env!("CARGO_MANIFEST_DIR")
        )
    }

    // ---- THE REGRESSION TEST ---------------------------------------------

    /// The isolation test the original defect needed and never had.
    ///
    /// Before this rewrite, `RuleEngine::evaluate` ignored the graph and used a
    /// hardcoded ladder. Every existing test still passed, because the shipped
    /// graph's thresholds happened to match that ladder. The only way to detect
    /// it is to change the graph and nothing else, then assert the OUTPUT
    /// changed — which is what this does.
    ///
    /// If someone reintroduces a hardcoded ladder or a fallback band, both
    /// assertions below cannot hold at once, so this test fails.
    #[test]
    fn test_graph_thresholds_actually_drive_severity() {
        let lenient = standard_engine();
        let strict = RuleEngine::from_json(
            &graph_with(&[
                row("[0..tolerance_cents]", "info", false),
                row("(tolerance_cents..tolerance_cents*2]", "medium", true),
                row("(tolerance_cents*2..]", "high", true),
            ]),
            "test_strict",
        )
        .expect("strict three-band graph must compile");

        // Identical input to both engines: a 500¢ variance at a 100¢ tolerance.
        let l = eval(&lenient, 500, 100);
        let s = eval(&strict, 500, 100);

        // 500 is in (100..1000] under the lenient table.
        assert_eq!(l.severity, "low", "lenient graph band (t..t*10] => low");
        // 500 is above 2*100 under the strict table.
        assert_eq!(s.severity, "high", "strict graph band (t*2..] => high");
        assert_ne!(
            l.severity, s.severity,
            "severity must come from the graph; equal severities here mean the \
             graph is being ignored again"
        );

        // And the recorded provenance differs, so an audit trail can tell the
        // two policies apart. (rule_version lives on the engine, not the output:
        // grpc/mod.rs copies it from the engine onto every result.)
        assert_ne!(
            lenient.rule_version, strict.rule_version,
            "different graphs must hash to different rule_versions"
        );
        assert_eq!(lenient.rule_count(), 4);
        assert_eq!(strict.rule_count(), 3);
    }

    /// A graph with no decision table used to evaluate successfully.
    ///
    /// `test_rule_engine_evaluate` in the previous version of this file built an
    /// engine from exactly this JSON and asserted `severity == "low"` for
    /// variance 5 / tolerance 1. That test passed. It could only pass because
    /// the graph was never consulted, and it is the clearest single piece of
    /// evidence the bug existed.
    #[test]
    fn test_empty_graph_must_not_load() {
        let result = RuleEngine::from_json(r#"{"nodes":[],"edges":[]}"#, "empty");
        match result {
            Err(ZenError::LoadError(m)) => assert!(
                m.contains("decisionTableNode"),
                "error should name what is missing, got: {}",
                m
            ),
            Err(other) => panic!("expected LoadError, got {:?}", other),
            Ok(_) => panic!(
                "a graph with zero rules must not load — this is the exact JSON \
                 that previously produced a confident \"low\" severity"
            ),
        }
    }

    // ---- band boundaries --------------------------------------------------
    //
    // Every assertion below is byte-for-byte the expectation the pre-rewrite
    // tests made against the hardcoded ladder. They are unchanged on purpose:
    // the ladder and the shipped graph agreed, which is why the bug hid. What
    // changed is that these now flow through the compiled graph, so they are
    // finally testing the thing the audit trail claims produced them.

    #[test]
    fn test_exact_tolerance_is_info() {
        let r = eval(&standard_engine(), 1, 1);
        assert_eq!(r.severity, "info");
        assert!(!r.exceeds_tolerance);
    }

    #[test]
    fn test_one_cent_over_is_low() {
        let r = eval(&standard_engine(), 2, 1);
        assert_eq!(r.severity, "low");
        assert!(r.exceeds_tolerance);
    }

    #[test]
    fn test_exactly_at_10x_is_low() {
        let r = eval(&standard_engine(), 10, 1);
        assert_eq!(r.severity, "low");
    }

    #[test]
    fn test_one_over_10x_is_medium() {
        let r = eval(&standard_engine(), 11, 1);
        assert_eq!(r.severity, "medium");
        assert!(r.exceeds_tolerance);
    }

    #[test]
    fn test_exactly_at_100x_is_medium() {
        let r = eval(&standard_engine(), 100, 1);
        assert_eq!(r.severity, "medium");
    }

    #[test]
    fn test_one_over_100x_is_high() {
        let r = eval(&standard_engine(), 101, 1);
        assert_eq!(r.severity, "high");
        assert!(r.exceeds_tolerance);
    }

    #[test]
    fn test_zero_variance_is_info() {
        let r = eval(&standard_engine(), 0, 1);
        assert_eq!(r.severity, "info");
        assert!(!r.exceeds_tolerance);
    }

    #[test]
    fn test_negative_variance_uses_absolute_value() {
        // |-50| = 50, t=10 => (10..100] => low. Direction must not change the
        // band: a 50¢ shortfall and a 50¢ overage are the same exception size.
        let r = eval(&standard_engine(), -50, 10);
        assert_eq!(r.severity, "low");
        assert!(r.exceeds_tolerance);
        let mirrored = eval(&standard_engine(), 50, 10);
        assert_eq!(r.severity, mirrored.severity);
    }

    #[test]
    fn test_zero_tolerance_zero_variance_is_info() {
        // [0..0] contains 0.
        let r = eval(&standard_engine(), 0, 0);
        assert_eq!(r.severity, "info");
        assert!(!r.exceeds_tolerance);
    }

    #[test]
    fn test_zero_tolerance_any_variance_is_high() {
        // t=0 collapses the three lower bands to empty: (0..0], (0..0] match
        // nothing, so 1¢ lands in (0..] => high. A zero-tolerance book treats
        // any difference as maximal, which is the intended reading.
        let r = eval(&standard_engine(), 1, 0);
        assert_eq!(r.severity, "high");
        assert!(r.exceeds_tolerance);
    }

    #[test]
    fn test_riverside_row2_variance() {
        // Riverside Design Co. row 2: bank -89900, GL +89400, invoice +89900.
        // Three-way variance 500¢, tolerance 100¢ => 5x => low.
        let r = eval(&standard_engine(), 500, 100);
        assert_eq!(r.severity, "low");
        assert!(r.exceeds_tolerance);
    }

    #[test]
    fn test_riverside_row1_zero_variance() {
        // Row 1 (many-to-many): 34250 + 12875 = 47125 = bank = GL, 0¢ gap.
        let r = eval(&standard_engine(), 0, 100);
        assert_eq!(r.severity, "info");
        assert!(!r.exceeds_tolerance);
    }

    // ---- the shipped graph ------------------------------------------------

    #[test]
    fn test_real_decision_graph_loads_and_compiles() {
        let engine = RuleEngine::new(&real_graph_path())
            .expect("the shipped decision graph must compile");
        assert_eq!(engine.rule_version.len(), 16);
        assert!(engine.rule_id.contains("gl_reconciliation"));
        assert_eq!(
            engine.rule_count(),
            4,
            "gl_reconciliation.json declares four bands; loading Ok is not \
             evidence they were compiled"
        );
    }

    #[test]
    fn test_real_decision_graph_evaluates() {
        let engine = RuleEngine::new(&real_graph_path()).expect("must compile");
        let r = eval(&engine, 50, 10);
        assert_eq!(r.severity, "low");
        assert!(r.exceeds_tolerance);
    }

    /// Pins the shipped policy to what these tests assert about it.
    ///
    /// If someone edits decision-graphs/gl_reconciliation.json, this fails. That
    /// is intended: this file is liability-critical policy, and a change to it
    /// should be a deliberate edit to the expectations too, not a silent one.
    #[test]
    fn test_real_graph_matches_the_bands_these_tests_assume() {
        let real = RuleEngine::new(&real_graph_path()).expect("must compile");
        assert_eq!(
            real.describe(),
            standard_engine().describe(),
            "the shipped graph no longer matches standard_rules() in this test \
             module; update both together and review the boundary assertions"
        );
    }

    // ---- operand grammar --------------------------------------------------

    #[test]
    fn test_operand_forms() {
        assert_eq!(
            Operand::parse("0").expect("int"),
            Operand { tol_coeff: 0, konst: 0 }
        );
        assert_eq!(
            Operand::parse("250").expect("int"),
            Operand { tol_coeff: 0, konst: 250 }
        );
        assert_eq!(
            Operand::parse("tolerance_cents").expect("ident"),
            Operand { tol_coeff: 1, konst: 0 }
        );
        assert_eq!(
            Operand::parse("tolerance_cents*10").expect("ident*n"),
            Operand { tol_coeff: 10, konst: 0 }
        );
        assert_eq!(
            Operand::parse("100*tolerance_cents").expect("n*ident"),
            Operand { tol_coeff: 100, konst: 0 }
        );
        // Whitespace is not significant.
        assert_eq!(
            Operand::parse(" tolerance_cents * 5 ").expect("spaced"),
            Operand { tol_coeff: 5, konst: 0 }
        );
        // Two constants fold.
        assert_eq!(
            Operand::parse("5*2").expect("n*n"),
            Operand { tol_coeff: 0, konst: 10 }
        );
    }

    #[test]
    fn test_operand_rejects_nonlinear_and_garbage() {
        for bad in [
            "tolerance_cents*tolerance_cents", // quadratic
            "abc",                             // unknown identifier
            "1.5",                             // no fractional cents in a bound
            "1*2*3",                           // too many factors
            "",                                // empty
        ] {
            assert!(
                Operand::parse(bad).is_err(),
                "'{}' must not parse as a bound operand",
                bad
            );
        }
    }

    #[test]
    fn test_parse_range_forms() {
        let (lo, hi) = parse_range("[0..tolerance_cents]").expect("closed");
        assert!(lo.is_inclusive() && hi.is_inclusive());
        let (lo, hi) = parse_range("(tolerance_cents..tolerance_cents*10]").expect("half-open");
        assert!(!lo.is_inclusive() && hi.is_inclusive());
        let (_, hi) = parse_range("(tolerance_cents*100..]").expect("unbounded above");
        assert!(matches!(hi, Bound::Unbounded));
        let (lo, _) = parse_range("[..100)").expect("unbounded below");
        assert!(matches!(lo, Bound::Unbounded));
    }

    #[test]
    fn test_parse_range_rejects_malformed() {
        for bad in [
            "0..tolerance_cents",  // no brackets
            "[0-tolerance_cents]", // no ".."
            "{0..1}",              // wrong brackets
            "[]",                  // too short
            "[0..1\u{20ac}]",      // non-ASCII (escaped so this file stays ASCII)
        ] {
            assert!(parse_range(bad).is_err(), "'{}' must not parse", bad);
        }
    }

    // ---- graph validation -------------------------------------------------

    fn expect_load_error(json: &str, needle: &str) {
        match RuleEngine::from_json(json, "t") {
            Err(ZenError::LoadError(m)) => assert!(
                m.contains(needle),
                "error {:?} should mention {:?}",
                m,
                needle
            ),
            Err(other) => panic!("expected LoadError, got {:?}", other),
            Ok(_) => panic!("expected load failure, got a usable engine"),
        }
    }

    #[test]
    fn test_bands_must_start_at_zero() {
        expect_load_error(
            &graph_with(&[
                row("[1..tolerance_cents]", "info", false),
                row("(tolerance_cents..]", "high", true),
            ]),
            "must start at inclusive 0",
        );
    }

    #[test]
    fn test_bands_must_end_unbounded() {
        expect_load_error(
            &graph_with(&[
                row("[0..tolerance_cents]", "info", false),
                row("(tolerance_cents..tolerance_cents*10]", "low", true),
            ]),
            "must be unbounded above",
        );
    }

    #[test]
    fn test_bands_must_not_leave_a_gap() {
        expect_load_error(
            &graph_with(&[
                row("[0..tolerance_cents]", "info", false),
                row("(tolerance_cents*10..]", "high", true),
            ]),
            "do not meet",
        );
    }

    #[test]
    fn test_bands_must_not_overlap() {
        expect_load_error(
            &graph_with(&[
                row("[0..tolerance_cents]", "info", false),
                row("[tolerance_cents..]", "high", true),
            ]),
            "both include",
        );
    }

    #[test]
    fn test_bands_must_not_exclude_their_shared_bound() {
        expect_load_error(
            &graph_with(&[
                row("[0..tolerance_cents)", "info", false),
                row("(tolerance_cents..]", "high", true),
            ]),
            "neither includes",
        );
    }

    #[test]
    fn test_negative_bound_rejected() {
        expect_load_error(
            &graph_with(&[
                row("[0..tolerance_cents]", "info", false),
                row("(tolerance_cents..-5]", "low", true),
                row("(-5..]", "high", true),
            ]),
            "negative bound",
        );
    }

    #[test]
    fn test_severity_must_match_db_check_constraint() {
        expect_load_error(
            &graph_with(&[
                row("[0..tolerance_cents]", "informational", false),
                row("(tolerance_cents..]", "high", true),
            ]),
            "not one of",
        );
    }

    #[test]
    fn test_exceeds_tolerance_must_be_boolean_text() {
        // Plain literal, not format!(): clippy::useless_format denies a format!
        // with no arguments and CI runs clippy with -D warnings.
        let json = r#"{"nodes":[{"id":"t","type":"decisionTableNode","name":"T","content":{"rules":[
            {"variance_cents":"[0..tolerance_cents]","severity":"info","exceeds_tolerance":"maybe"},
            {"variance_cents":"(tolerance_cents..]","severity":"high","exceeds_tolerance":"true"}
        ]}}]}"#;
        expect_load_error(json, "must be");
    }

    #[test]
    fn test_two_decision_tables_rejected() {
        let json = r#"{"nodes":[
            {"id":"a","type":"decisionTableNode","name":"First","content":{"rules":[
                {"variance_cents":"[0..]","severity":"info","exceeds_tolerance":"false"}]}},
            {"id":"b","type":"decisionTableNode","name":"Second","content":{"rules":[
                {"variance_cents":"[0..]","severity":"high","exceeds_tolerance":"true"}]}}
        ]}"#;
        expect_load_error(json, "exactly one is");
    }

    #[test]
    fn test_decision_table_without_content_rejected() {
        expect_load_error(
            r#"{"nodes":[{"id":"t","type":"decisionTableNode","name":"T"}]}"#,
            "has no content",
        );
    }

    #[test]
    fn test_decision_table_with_zero_rules_rejected() {
        expect_load_error(
            r#"{"nodes":[{"id":"t","type":"decisionTableNode","name":"T","content":{"rules":[]}}]}"#,
            "zero rules",
        );
    }

    #[test]
    fn test_invalid_json_and_bad_path() {
        assert!(RuleEngine::from_json("not json", "bad").is_err());
        assert!(RuleEngine::new("/nonexistent/path.json").is_err());
    }

    // ---- arithmetic safety ------------------------------------------------

    #[test]
    fn test_tolerance_overflow_is_an_error_not_a_wrapped_threshold() {
        // t chosen so t*10 overflows i64. Band 1 ([0..t]) does not match a
        // variance of i64::MAX, so evaluation reaches band 2's t*10 bound.
        let t = i64::MAX / 5;
        let engine = standard_engine();
        let result = engine.evaluate(&ReconciliationInput {
            variance_cents: i64::MAX,
            tolerance_cents: t,
        });
        match result {
            Err(ZenError::EvalError(m)) => {
                assert!(m.contains("overflows"), "got: {}", m)
            }
            Err(other) => panic!("expected EvalError, got {:?}", other),
            Ok(out) => panic!(
                "overflow silently produced severity {:?}; a wrapped negative \
                 threshold would reclassify every variance",
                out.severity
            ),
        }
    }

    #[test]
    fn test_i64_min_variance_is_an_error_not_a_panic() {
        // i64::MIN.abs() panics in debug and wraps in release. Neither may reach
        // a severity band.
        let engine = standard_engine();
        let result = engine.evaluate(&ReconciliationInput {
            variance_cents: i64::MIN,
            tolerance_cents: 100,
        });
        assert!(matches!(result, Err(ZenError::EvalError(_))));
    }

    // ---- rule_version -----------------------------------------------------

    #[test]
    fn test_rule_version_is_stable_and_content_addressed() {
        let a = r#"{"nodes":[{"id":"a"}]}"#;
        let b = r#"{"nodes":[{"id":"b"}]}"#;
        assert_eq!(
            compute_rule_version(a.as_bytes()),
            compute_rule_version(a.as_bytes())
        );
        assert_ne!(
            compute_rule_version(a.as_bytes()),
            compute_rule_version(b.as_bytes())
        );
        let v = compute_rule_version(a.as_bytes());
        assert_eq!(v.len(), 16);
        assert!(v.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn test_describe_reports_the_bands_in_force() {
        let d = standard_engine().describe();
        assert!(d.contains("[0..tolerance_cents] => info"), "got: {}", d);
        assert!(
            d.contains("(tolerance_cents*100..] => high/exceeds"),
            "got: {}",
            d
        );
    }
}
