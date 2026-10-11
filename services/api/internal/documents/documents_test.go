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
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

func TestConfirmUploadAppliesHeaderSniff(t *testing.T) {
	// SOURCE-INVARIANT: HandleConfirmUpload needs storage + a live DB, so the
	// behavioural equivalent cannot run here. The presign path's
	// caller-specified doc_type is the same class as rule 20's bug (rule 20's
	// fix covers the direct path); this pins the fix on the presign side: the
	// confirm handler must run the same sniff — the bytes are buffered there
	// for hashing — and record the corrected doc_type in the same UPDATE as
	// the content hash, before the ingestion trigger carries doc_type onward.
	raw, err := os.ReadFile("documents.go")
	if err != nil {
		t.Fatalf("documents.go unreadable: %v", err)
	}
	src := string(raw)
	confirmPos := strings.Index(src, "func (s *Service) HandleConfirmUpload")
	if confirmPos == -1 {
		t.Fatal("HandleConfirmUpload not found in documents.go")
	}
	body := src[confirmPos:]
	if !strings.Contains(body, "refineDocTypeFromHeader(data)") {
		t.Error("HandleConfirmUpload does not run the header sniff — the presign path's caller-specified doc_type reaches ingestion unrefined (rule 20's class, second instance)")
	}
	if !strings.Contains(body, "SET content_hash = $1, ocr_status = 'pending', doc_type = $2") {
		t.Error("the confirm UPDATE does not record the corrected doc_type — a refined doc_type that is never written leaves the row wrong forever (traceability)")
	}
}

// buildTestWorkbook constructs a minimal xlsx (a zip with sharedStrings +
// sheet1.xml). The repo's only xlsx fixture is ASCII text, not a zip, so
// there is no real-workbook fixture to read.
func buildTestWorkbook(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	ss, err := zw.Create("xl/sharedStrings.xml")
	if err != nil {
		t.Fatalf("create sharedStrings: %v", err)
	}
	if _, err := ss.Write([]byte(`<?xml version="1.0"?><sst count="5" uniqueCount="5"><si><t>Date</t></si><si><t>Amount</t></si><si><t>Description</t></si><si><t>Counterparty</t></si><si><t>Currency</t></si></sst>`)); err != nil {
		t.Fatalf("write sharedStrings: %v", err)
	}
	sh, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("create sheet1: %v", err)
	}
	if _, err := sh.Write([]byte(`<?xml version="1.0"?><worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c><c r="E1" t="s"><v>4</v></c></row><row r="2"><c r="A2" t="s"><v>5</v></c></row></worksheet>`)); err != nil {
		t.Fatalf("write sheet1: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestXlsxHeaderRow_ExtractsFirstRow(t *testing.T) {
	got := xlsxHeaderRow(buildTestWorkbook(t))
	want := []string{"Date", "Amount", "Description", "Counterparty", "Currency"}
	if len(got) != len(want) {
		t.Fatalf("header row: got %d cells (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestXlsxHeaderRow_NotAZipReturnsNil(t *testing.T) {
	// The shipped sample_gl.xlsx fixture is ASCII text, not a zip: the open
	// fails and nil is returned, so the caller falls back to the CSV text
	// extraction.
	if got := xlsxHeaderRow([]byte("Transaction Date,Num,Name,Account,Debit,Credit\n")); got != nil {
		t.Errorf("text content: got %v, want nil", got)
	}
}

func TestXlsxUploadRefinesInvoiceWorkbook(t *testing.T) {
	// The built workbook's header classifies as invoice (counterparty present,
	// no account column) — the .xlsx branch of the sniff, one classifyHeader
	// implementation feeding both extraction paths.
	got := classifyHeader(xlsxHeaderRow(buildTestWorkbook(t)))
	if got != "invoice" {
		t.Errorf("workbook header: classified %q, want invoice", got)
	}
}
