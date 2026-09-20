package documents

// documents_test.go — the doc_type refinement guard.
//
// refineDocTypeFromHeader classifies a CSV's header row into a doc type at
// upload. Without it, allowedDocTypes maps every .csv to gl_export, an
// invoice delivered as CSV is ingested as GL, and its rows land as gl_entry —
// invoice_line_item is 0 for any book whose invoices arrive this way and the
// 3-way reconciliation never assembles (observed live 2026-09-20:
// sample_invoice.csv uploaded twice to the demo book and recorded gl_export
// both times; the book's extracted_entities held 50 gl_entry and 20
// bank_transaction and 0 invoice_line_item; every link pass refused to
// assemble, with ocr_status='done' everywhere and no error anywhere).
//
// The fixture-based cases read the ACTUAL fixture files under
// services/ingestion/test_fixtures — the same trade
// test_fixtures_are_still_sign_aligned makes: a re-headed fixture that
// changes the answer goes red here instead of the rule quietly rotting.
//
// These tests have never been compiled in the environment that wrote them
// (no Go toolchain there); the first `go test ./internal/documents/` on the
// user's Mac is the check. Their expected values were re-derived by a Python
// port run against the same fixture files.

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	// THREE levels up: Go tests run with the CWD set to the package directory
	// (services/api/internal/documents), so ../.. is services/api and
	// ../../ingestion resolves to services/api/ingestion — which does not
	// exist. The first version of this test had exactly that bug and failed on
	// the user's Mac with "fixture unreadable" while the other three tests
	// passed.
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "ingestion", "test_fixtures", name))
	if err != nil {
		t.Fatalf("%s: fixture unreadable: %v", name, err)
	}
	return data
}

func TestRefineDocTypeFromHeader_FixturesClassifyCorrectly(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		// the shipped invoice fixture: counterparty present, no account column
		{"sample_invoice.csv", "invoice"},
		// both shipped GL fixtures carry an account column
		{"sample_gl.csv", "gl_export"},
		{"sample_gl_xero.csv", "gl_export"},
	}
	for _, tc := range cases {
		got := refineDocTypeFromHeader(fixture(t, tc.file))
		if got != tc.want {
			t.Errorf("%s: refined %q, want %q — the fixture header changed or the sniff drifted", tc.file, got, tc.want)
		}
	}
}

func TestRefineDocTypeFromHeader_UnsignaledHeaderKeepsDefault(t *testing.T) {
	// "" means "no signal — the caller keeps the extension default", so an
	// unknown export behaves exactly as it did before the refinement existed.
	for _, header := range []string{
		"a,b,c\n1,2,3\n",
		"date,description,amount,balance\n", // bank-style CSV: no account, no invoice signal
		"",
	} {
		if got := refineDocTypeFromHeader([]byte(header)); got != "" {
			t.Errorf("header %q: refined %q, want \"\" (no signal keeps the extension default)", header, got)
		}
	}
}

func TestRefineDocTypeFromHeader_AccountColumnWinsOverInvoiceSignal(t *testing.T) {
	// A header carrying BOTH signals refines to gl_export: the account column
	// is the stronger signal, and the Xero GL header carries counterparty-ish
	// "Contact Name" beside "Account"/"Account Code" — the invoice signal
	// must not override it.
	data := []byte("Date,Contact Name,Description,Account,Amount\n2024-01-15,Acme,Consulting,4000,1500.00\n")
	if got := refineDocTypeFromHeader(data); got != "gl_export" {
		t.Errorf("both-signals header: refined %q, want gl_export (account column wins)", got)
	}
}

func TestRefineDocTypeFromHeader_IsCaseInsensitiveAndQuoted(t *testing.T) {
	// Quoted + mixed-case headers must read the same as bare lowercase ones,
	// and every invoice synonym fires.
	data := []byte("\"Date\",\"Amount\",\"Description\",\"Counterparty\",\"Currency\"\n2024-01-15,1500.00,Consulting,Acme,USD\n")
	if got := refineDocTypeFromHeader(data); got != "invoice" {
		t.Errorf("quoted mixed-case invoice header: refined %q, want invoice", got)
	}
	for _, header := range []string{
		"invoice_number,amount,customer\n",
		"INVOICE DATE,AMOUNT\n",
	} {
		if got := refineDocTypeFromHeader([]byte(header)); got != "invoice" {
			t.Errorf("header %q: refined %q, want invoice", header, got)
		}
	}
}
