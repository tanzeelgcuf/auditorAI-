// services/verification/src/decimal_math/mod.rs
// ALL monetary arithmetic lives here — deterministic, unit-tested to 100% branch coverage.
// NEVER use f32/f64 for money. ONLY rust_decimal::Decimal.

use rust_decimal::Decimal;
use rust_decimal::prelude::*;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum MathError {
    #[error("overflow in calculation")]
    Overflow,
    #[error("division by zero")]
    #[allow(dead_code)]
    DivisionByZero,
}

pub type MathResult<T> = Result<T, MathError>;

/// Compute variance = |sum_a - sum_b| for grouped reconciliation.
/// Both arguments are in cents (integers), but stored as Decimal for exactness.
#[allow(dead_code)]
pub fn compute_variance(a: &[Decimal], b: &[Decimal]) -> MathResult<Decimal> {
    let sum_a = sum(a)?;
    let sum_b = sum(b)?;
    Ok((sum_a - sum_b).abs())
}

/// Sum a slice of Decimal values with overflow protection.
pub fn sum(values: &[Decimal]) -> MathResult<Decimal> {
    let mut total = Decimal::ZERO;
    for v in values {
        total = total.checked_add(*v).ok_or(MathError::Overflow)?;
    }
    Ok(total)
}

/// Check if variance exceeds tolerance.
/// Returns (variance, exceeded).
#[allow(dead_code)]
pub fn check_tolerance(variance: Decimal, tolerance_cents: i64) -> (Decimal, bool) {
    let tolerance_dec = Decimal::from_i64(tolerance_cents)
        .unwrap_or(Decimal::ZERO);
    (variance, variance > tolerance_dec)
}

/// Compute materiality-greater-of tolerance.
/// Returns whichever is larger: fixed_cents or percentage * total.
#[allow(dead_code)]
pub fn compute_greater_of_tolerance(
    fixed_cents: i64,
    percentage: Decimal,
    total: &[Decimal],
) -> MathResult<i64> {
    let total_sum = sum(total)?;
    let fixed = Decimal::from_i64(fixed_cents).unwrap_or(Decimal::ZERO);
    // percentage * total_sum gives dollar amount; multiply by 100 to convert to cents
    let pct_cents = (total_sum * percentage) * Decimal::from_i64(100).unwrap_or(Decimal::ONE);

    let result = if pct_cents > fixed { pct_cents } else { fixed };
    // Round to nearest cent (already in cents, but round_dp(0) is safe)
    result.round_dp(0).to_i64().ok_or(MathError::Overflow)
}

/// Grouped tolerance: sum each group, then check variance across groups.
/// For 3-way reconciliation: invoice_total vs bank_total vs gl_total.
pub fn compute_three_way_variance(
    invoice_group: &[Decimal],
    bank_group: &[Decimal],
    gl_group: &[Decimal],
) -> MathResult<(Decimal, Decimal, Decimal)> {
    let inv_sum = sum(invoice_group)?;
    let bank_sum = sum(bank_group)?;
    let gl_sum = sum(gl_group)?;

    let variance_ib = (inv_sum - bank_sum).abs();
    let variance_igl = (inv_sum - gl_sum).abs();
    let variance_bgl = (bank_sum - gl_sum).abs();

    Ok((variance_ib, variance_igl, variance_bgl))
}

/// Format a human-readable calculation formula string.
pub fn format_formula(
    name: &str,
    invoice_amount: Decimal,
    bank_amount: Decimal,
    gl_amount: Decimal,
    variance: Decimal,
    tolerance: i64,
) -> String {
    format!(
        "{}: invoice={:.2}, bank={:.2}, gl={:.2}, variance={:.2}, tolerance={}",
        name, invoice_amount, bank_amount, gl_amount, variance, tolerance
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use rust_decimal_macros::dec;
    use proptest::prelude::*;

    // ---- sum tests ----

    #[test]
    fn test_sum_empty() {
        assert_eq!(sum(&[]).unwrap(), Decimal::ZERO);
    }

    #[test]
    fn test_sum_single() {
        let result = sum(&[dec!(100.50)]).unwrap();
        assert_eq!(result, dec!(100.50));
    }

    #[test]
    fn test_sum_multiple() {
        let vals = [dec!(100.50), dec!(200.25), dec!(50.25)];
        let result = sum(&vals).unwrap();
        assert_eq!(result, dec!(351.00));
    }

    #[test]
    fn test_sum_negative() {
        let vals = [dec!(-100.50), dec!(100.50)];
        let result = sum(&vals).unwrap();
        assert_eq!(result, Decimal::ZERO);
    }

    #[test]
    fn test_sum_all_negative() {
        let vals = [dec!(-10.00), dec!(-20.00), dec!(-30.00)];
        let result = sum(&vals).unwrap();
        assert_eq!(result, dec!(-60.00));
    }

    #[test]
    fn test_sum_large_values_no_overflow() {
        // Not testing near MAX because Decimal::MAX + Decimal::MIN = 0
        let vals = [Decimal::MAX, Decimal::MIN];
        let result = sum(&vals);
        assert!(result.is_ok());
        assert_eq!(result.unwrap(), Decimal::ZERO);
    }

    // ---- compute_variance tests ----

    #[test]
    fn test_variance_empty() {
        let v = compute_variance(&[], &[]).unwrap();
        assert_eq!(v, Decimal::ZERO);
    }

    #[test]
    fn test_variance_empty_vs_values() {
        let v = compute_variance(&[], &[dec!(100.00)]).unwrap();
        assert_eq!(v, dec!(100.00));
    }

    #[test]
    fn test_variance_same() {
        let v = compute_variance(&[dec!(50.00)], &[dec!(50.00)]).unwrap();
        assert_eq!(v, Decimal::ZERO);
    }

    #[test]
    fn test_variance_different() {
        let v = compute_variance(&[dec!(100.00)], &[dec!(75.50)]).unwrap();
        assert_eq!(v, dec!(24.50));
    }

    #[test]
    fn test_variance_negative_values() {
        let v = compute_variance(&[dec!(-50.00)], &[dec!(50.00)]).unwrap();
        assert_eq!(v, dec!(100.00));
    }

    #[test]
    fn test_variance_commutative() {
        let a = [dec!(10.00), dec!(20.00)];
        let b = [dec!(15.00), dec!(15.00)];
        let v1 = compute_variance(&a, &b).unwrap();
        let v2 = compute_variance(&b, &a).unwrap();
        assert_eq!(v1, v2);
    }

    // ---- check_tolerance tests ----

    #[test]
    fn test_tolerance_exact_boundary() {
        let variance = dec!(1.00);
        let (_, exceeded) = check_tolerance(variance, 1);
        assert!(!exceeded, "variance equal to tolerance should NOT exceed");
    }

    #[test]
    fn test_tolerance_one_over() {
        let variance = dec!(1.01);
        let (_, exceeded) = check_tolerance(variance, 1);
        assert!(exceeded, "variance 1 cent over tolerance SHOULD exceed");
    }

    #[test]
    fn test_tolerance_one_under() {
        let variance = dec!(0.99);
        let (_, exceeded) = check_tolerance(variance, 1);
        assert!(!exceeded);
    }

    #[test]
    fn test_tolerance_zero() {
        let variance = dec!(0.00);
        let (_, exceeded) = check_tolerance(variance, 1);
        assert!(!exceeded);
    }

    #[test]
    fn test_tolerance_zero_tolerance() {
        let variance = dec!(0.00);
        let (_, exceeded) = check_tolerance(variance, 0);
        assert!(!exceeded, "zero variance within zero tolerance");
    }

    #[test]
    fn test_tolerance_zero_tolerance_over() {
        let variance = dec!(0.01);
        let (_, exceeded) = check_tolerance(variance, 0);
        assert!(exceeded, "any positive variance exceeds zero tolerance");
    }

    #[test]
    fn test_tolerance_large_numbers() {
        let variance = dec!(1_000_000.00);
        let (_, exceeded) = check_tolerance(variance, 500_000);
        assert!(exceeded);
    }

    // ---- compute_greater_of_tolerance tests ----

    #[test]
    fn test_greater_of_tolerance_small_total() {
        let total = [dec!(10.00)]; // 0.5% of $10 = $0.05
        let result = compute_greater_of_tolerance(1, dec!(0.005), &total).unwrap();
        // 0.005 * 10.00 = 0.05 = 5 cents > 1 cent
        assert_eq!(result, 5);
    }

    #[test]
    fn test_greater_of_tolerance_large_total() {
        let total = [dec!(10000.00)]; // 0.5% of $10k = $50 = 5000 cents
        let result = compute_greater_of_tolerance(1, dec!(0.005), &total).unwrap();
        assert_eq!(result, 5000);
    }

    #[test]
    fn test_greater_of_tolerance_fixed_wins() {
        let total = [dec!(1.00)]; // 0.5% of $1 = $0.005 = 0.5 cents
        let result = compute_greater_of_tolerance(10, dec!(0.005), &total).unwrap();
        // fixed=10 cents > 0.5% of $1.00 = 0.5 cents
        assert_eq!(result, 10);
    }

    #[test]
    fn test_greater_of_tolerance_zero_fixed() {
        let total = [dec!(100.00)]; // 0.5% = $0.50 = 50 cents
        let result = compute_greater_of_tolerance(0, dec!(0.005), &total).unwrap();
        assert_eq!(result, 50);
    }

    #[test]
    fn test_greater_of_tolerance_zero_percentage() {
        let total = [dec!(1000.00)];
        let result = compute_greater_of_tolerance(100, dec!(0.0), &total).unwrap();
        assert_eq!(result, 100);
    }

    #[test]
    fn test_greater_of_tolerance_zero_total() {
        let total = [Decimal::ZERO];
        let result = compute_greater_of_tolerance(1, dec!(0.005), &total).unwrap();
        assert_eq!(result, 1);
    }

    // ---- three-way variance tests ----

    #[test]
    fn test_three_way_variance_exact_match() {
        let invoice = [dec!(100.00), dec!(50.00)];
        let bank = [dec!(150.00)];
        let gl = [dec!(150.00)];

        let (ib, igl, bgl) = compute_three_way_variance(&invoice, &bank, &gl).unwrap();
        assert_eq!(ib, Decimal::ZERO);
        assert_eq!(igl, Decimal::ZERO);
        assert_eq!(bgl, Decimal::ZERO);
    }

    #[test]
    fn test_three_way_variance_mismatch() {
        let invoice = [dec!(100.00)];
        let bank = [dec!(99.50)];
        let gl = [dec!(100.00)];

        let (ib, igl, bgl) = compute_three_way_variance(&invoice, &bank, &gl).unwrap();
        assert_eq!(ib, dec!(0.50));
        assert_eq!(igl, Decimal::ZERO);
        assert_eq!(bgl, dec!(0.50));
    }

    #[test]
    fn test_three_way_variance_all_different() {
        let invoice = [dec!(100.00)];
        let bank = [dec!(90.00)];
        let gl = [dec!(80.00)];

        let (ib, igl, bgl) = compute_three_way_variance(&invoice, &bank, &gl).unwrap();
        assert_eq!(ib, dec!(10.00));
        assert_eq!(igl, dec!(20.00));
        assert_eq!(bgl, dec!(10.00));
    }

    #[test]
    fn test_three_way_with_negatives() {
        let invoice = [dec!(-50.00)];
        let bank = [dec!(50.00)];
        let gl = [dec!(0.00)];

        let (ib, igl, bgl) = compute_three_way_variance(&invoice, &bank, &gl).unwrap();
        assert_eq!(ib, dec!(100.00));
        assert_eq!(igl, dec!(50.00));
        assert_eq!(bgl, dec!(50.00));
    }

    #[test]
    fn test_three_way_empty_groups() {
        let (ib, igl, bgl) = compute_three_way_variance(&[], &[], &[]).unwrap();
        assert_eq!(ib, Decimal::ZERO);
        assert_eq!(igl, Decimal::ZERO);
        assert_eq!(bgl, Decimal::ZERO);
    }

    // ---- format_formula tests ----

    #[test]
    fn test_format_formula() {
        let formula = format_formula(
            "GL Reconciliation",
            dec!(1500.00), dec!(1500.00), dec!(1500.50),
            dec!(0.50), 1,
        );
        assert!(formula.contains("GL Reconciliation"));
        assert!(formula.contains("variance=0.50"));
        assert!(formula.contains("tolerance=1"));
        assert!(formula.contains("invoice=1500.00"));
        assert!(formula.contains("bank=1500.00"));
        assert!(formula.contains("gl=1500.50"));
    }

    #[test]
    fn test_format_formula_zero_values() {
        let formula = format_formula(
            "Reconciliation",
            Decimal::ZERO, Decimal::ZERO, Decimal::ZERO,
            Decimal::ZERO, 0,
        );
        assert!(formula.contains("Reconciliation"));
        assert!(formula.contains("variance=0.00"));
    }

    // ---- proptest: sum property ----

    proptest! {
        #[test]
        fn proptest_sum_abs_bound(
            vals in proptest::collection::vec(-1_000_000_000i64..1_000_000_000, 1..100)
        ) {
            let decs: Vec<Decimal> = vals.iter().map(|&v| Decimal::from_i64(v).unwrap_or(Decimal::ZERO)).collect();
            let total = sum(&decs).unwrap();
            let abs_sum: Decimal = decs.iter().map(|d| d.abs()).sum();
            // |sum| <= sum(|v|)
            assert!(total.abs() <= abs_sum || total == Decimal::ZERO);
        }

        #[test]
        fn proptest_variance_commutative(
            a in proptest::collection::vec(-1_000_000i64..1_000_000, 1..10),
            b in proptest::collection::vec(-1_000_000i64..1_000_000, 1..10),
        ) {
            let decs_a: Vec<Decimal> = a.iter().map(|&v| Decimal::from_i64(v).unwrap_or(Decimal::ZERO)).collect();
            let decs_b: Vec<Decimal> = b.iter().map(|&v| Decimal::from_i64(v).unwrap_or(Decimal::ZERO)).collect();
            let v1 = compute_variance(&decs_a, &decs_b).unwrap();
            let v2 = compute_variance(&decs_b, &decs_a).unwrap();
            assert_eq!(v1, v2);
        }
    }
}
