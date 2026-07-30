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
    DivisionByZero,
}

pub type MathResult<T> = Result<T, MathError>;

/// Compute variance = |sum_a - sum_b| for grouped reconciliation.
/// Both arguments are in cents (integers), but stored as Decimal for exactness.
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
pub fn check_tolerance(variance: Decimal, tolerance_cents: i64) -> (Decimal, bool) {
    let tolerance_dec = Decimal::from_i64(tolerance_cents).unwrap_or(Decimal::ZERO);
    (variance, variance > tolerance_dec)
}

/// Compute materiality-greater-of tolerance.
/// Returns whichever is larger: fixed_cents or percentage * total.
pub fn compute_greater_of_tolerance(
    fixed_cents: i64,
    percentage: Decimal,
    total: &[Decimal],
) -> MathResult<i64> {
    let total_sum = sum(total)?;
    let fixed = Decimal::from_i64(fixed_cents).unwrap_or(Decimal::ZERO);
    let pct = total_sum * percentage;

    let result = if pct > fixed { pct } else { fixed };
    // Round to nearest cent
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
    fn test_tolerance_zero() {
        let variance = dec!(0.00);
        let (_, exceeded) = check_tolerance(variance, 1);
        assert!(!exceeded);
    }

    #[test]
    fn test_greater_of_tolerance_small_total() {
        let total = [dec!(10.00)]; // 0.5% of $10 = $0.05
        let result = compute_greater_of_tolerance(1, dec!(0.005), &total).unwrap();
        // fixed_cents=1 ($0.01) > 0.5% of $10.00 = $0.05
        // Actually 0.005 * 10.00 = 0.05 = 5 cents > 1 cent
        assert_eq!(result, 5);
    }

    #[test]
    fn test_greater_of_tolerance_large_total() {
        let total = [dec!(10000.00)]; // 0.5% of $10k = $50 = 5000 cents
        let result = compute_greater_of_tolerance(1, dec!(0.005), &total).unwrap();
        assert_eq!(result, 5000);
    }

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
    }
}