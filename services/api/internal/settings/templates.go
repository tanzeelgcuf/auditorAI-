package settings

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedTemplates inserts the industry chart-of-accounts templates if the
// coa_templates table is empty. Idempotent: safe to call on every server
// start; an existing template is never overwritten.
func SeedTemplates(ctx context.Context, db *pgxpool.Pool) error {
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM coa_templates`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	for _, t := range coaTemplateSeeds {
		accounts, err := json.Marshal(t.Accounts)
		if err != nil {
			return err
		}
		_, err = db.Exec(ctx,
			`INSERT INTO coa_templates (template_name, industry, accounts) VALUES ($1, $2, $3)`,
			t.Name, t.Industry, accounts)
		if err != nil {
			return err
		}
	}
	slog.Info("seeded coa templates", "count", len(coaTemplateSeeds))
	return nil
}

type coaSeed struct {
	Name     string
	Industry string
	Accounts []templateAccount
}

// Retained Earnings is permanently non-reconcilable (a roll-forward equity
// balance). Retail/SaaS expose 15 accounts each; Nonprofit swaps owner
// equity for restricted/net-asset funds and grant revenue.
var coaTemplateSeeds = []coaSeed{
	{
		Name: "Standard Retail", Industry: "retail",
		Accounts: []templateAccount{
			{AccountCode: "1000", AccountName: "Cash - Operating", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1100", AccountName: "Accounts Receivable", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1200", AccountName: "Inventory", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1300", AccountName: "Prepaid Expenses", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "1400", AccountName: "Fixed Assets", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "2000", AccountName: "Accounts Payable", AccountType: "liability", IsReconcilable: true},
			{AccountCode: "2100", AccountName: "Accrued Expenses", AccountType: "liability", IsReconcilable: false},
			{AccountCode: "2200", AccountName: "Sales Tax Payable", AccountType: "liability", IsReconcilable: false},
			{AccountCode: "2300", AccountName: "Short-Term Debt", AccountType: "liability", IsReconcilable: true},
			{AccountCode: "3000", AccountName: "Owner's Equity", AccountType: "equity", IsReconcilable: false},
			{AccountCode: "3100", AccountName: "Retained Earnings", AccountType: "equity", IsReconcilable: false},
			{AccountCode: "4000", AccountName: "Sales Revenue", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "4100", AccountName: "Other Income", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "5000", AccountName: "Cost of Goods Sold", AccountType: "expense", IsReconcilable: true},
			{AccountCode: "5100", AccountName: "Operating Expenses", AccountType: "expense", IsReconcilable: true},
		},
	},
	{
		Name: "Standard Services/SaaS", Industry: "saas",
		Accounts: []templateAccount{
			{AccountCode: "1000", AccountName: "Cash - Operating", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1100", AccountName: "Accounts Receivable", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1200", AccountName: "Deferred Cost of Revenue", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "1300", AccountName: "Prepaid Expenses", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "1400", AccountName: "Fixed Assets", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "2000", AccountName: "Accounts Payable", AccountType: "liability", IsReconcilable: true},
			{AccountCode: "2100", AccountName: "Accrued Expenses", AccountType: "liability", IsReconcilable: false},
			{AccountCode: "2200", AccountName: "Deferred Revenue", AccountType: "liability", IsReconcilable: false},
			{AccountCode: "2300", AccountName: "Payroll Liabilities", AccountType: "liability", IsReconcilable: true},
			{AccountCode: "3000", AccountName: "Owner's Equity", AccountType: "equity", IsReconcilable: false},
			{AccountCode: "3100", AccountName: "Retained Earnings", AccountType: "equity", IsReconcilable: false},
			{AccountCode: "4000", AccountName: "Subscription Revenue", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "4100", AccountName: "Services Revenue", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "5000", AccountName: "Hosting & Infrastructure", AccountType: "expense", IsReconcilable: true},
			{AccountCode: "5100", AccountName: "Sales & Marketing", AccountType: "expense", IsReconcilable: true},
		},
	},
	{
		Name: "Standard Nonprofit", Industry: "nonprofit",
		Accounts: []templateAccount{
			{AccountCode: "1000", AccountName: "Cash - Operating", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1100", AccountName: "Grants Receivable", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1200", AccountName: "Pledges Receivable", AccountType: "asset", IsReconcilable: true},
			{AccountCode: "1300", AccountName: "Prepaid Expenses", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "1400", AccountName: "Restricted Investments", AccountType: "asset", IsReconcilable: false},
			{AccountCode: "2000", AccountName: "Accounts Payable", AccountType: "liability", IsReconcilable: true},
			{AccountCode: "2100", AccountName: "Accrued Expenses", AccountType: "liability", IsReconcilable: false},
			{AccountCode: "2200", AccountName: "Deferred Grant Revenue", AccountType: "liability", IsReconcilable: false},
			{AccountCode: "3000", AccountName: "Net Assets - Unrestricted", AccountType: "equity", IsReconcilable: false},
			{AccountCode: "3100", AccountName: "Net Assets - Temporarily Restricted", AccountType: "equity", IsReconcilable: false},
			{AccountCode: "4000", AccountName: "Grant Revenue", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "4100", AccountName: "Donation Revenue", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "4200", AccountName: "Program Service Revenue", AccountType: "revenue", IsReconcilable: true},
			{AccountCode: "5000", AccountName: "Program Expenses", AccountType: "expense", IsReconcilable: true},
			{AccountCode: "5100", AccountName: "Fundraising & Administrative", AccountType: "expense", IsReconcilable: true},
		},
	},
}
