package pipeline

// Column-mapping selection tests (Prompt C): a firm may hold multiple exports
// (QBO, Xero, custom) needing different maps; the coordinator picks by matching
// the file's header row against each mapping's source columns, not the most
// recent mapping (which sent a QBO export through a stress-set map).
import "testing"

func TestMappingHeaderScore_QBOBeatsStress(t *testing.T) {
	qbo := map[string]string{
		"date": "Date", "amount": "Debit", "description": "Memo",
		"account_code": "Account", "counterparty": "Name", "transaction_ref": "Num",
	}
	stress := map[string]string{
		"date": "posting_date", "amount": "debit_amount", "account": "gl_code",
		"description": "description", "counterparty": "vendor", "transaction_ref": "ref",
	}
	header := []string{"Date", "Type", "Doc Number", "Memo", "Name", "Account", "Debit", "Credit", "Amount"}
	qboScore := mappingHeaderScore(qbo, header)
	stressScore := mappingHeaderScore(stress, header)
	if qboScore <= stressScore {
		t.Errorf("QBO map (%d) should beat stress map (%d) on QBO headers", qboScore, stressScore)
	}
}

func TestMappingHeaderScore_StressBeatsQBO(t *testing.T) {
	qbo := map[string]string{"date": "Date", "amount": "Debit", "counterparty": "Name"}
	stress := map[string]string{"date": "posting_date", "amount": "debit_amount", "counterparty": "vendor"}
	header := []string{"posting_date", "debit_amount", "credit_amount", "gl_code", "ref", "vendor"}
	if mappingHeaderScore(stress, header) <= mappingHeaderScore(qbo, header) {
		t.Errorf("stress map should beat QBO map on stress headers")
	}
}

func TestMappingHeaderScore_CaseInsensitive(t *testing.T) {
	m := map[string]string{"amount": "Debit", "date": "DATE"}
	header := []string{"date", "debit", "amount"}
	if got := mappingHeaderScore(m, header); got != 2 {
		t.Errorf("case-insensitive match: got %d, want 2", got)
	}
}
