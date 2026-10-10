// The provider pulls: each returns []entityRecord — the provider's data
// mapped into the pipeline's entity shape. QuickBooks' data comes through
// its query endpoint (SELECT * FROM <Entity>); Xero's through its REST
// endpoints (api.xro/2.0/...). All amounts go through decimalToCents (no
// float). The GL legs are ONE ENTITY PER LINE in both providers' models — a
// line is a leg, which is what the presence rule (init.sql's BOOL_OR, the
// gRPC has_X flags) needs the group's legs to be.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// qboRecord is the union of the fields the sync reads across QBO's Invoice,
// JournalEntry, Purchase and Deposit records; JSON unmarshal ignores what a
// given record doesn't carry.
type qboRecord struct {
	Id       string `json:"Id"`
	TotalAmt string `json:"TotalAmt"`
	TxnDate  string `json:"TxnDate"`
	MetaData struct {
		CreateTime      string `json:"CreateTime"`
		LastUpdatedTime string `json:"LastUpdatedTime"`
	} `json:"MetaData"`
	CustomerRef struct {
		Name string `json:"name"`
	} `json:"CustomerRef"`
	VendorRef struct {
		Name string `json:"name"`
	} `json:"VendorRef"`
	Line []struct {
		Amount                 string `json:"Amount"`
		Description            string `json:"Description"`
		JournalEntryLineDetail struct {
			PostingType string `json:"PostingType"`
			AccountRef  struct {
				Name string `json:"name"`
			} `json:"AccountRef"`
		} `json:"JournalEntryLineDetail"`
	} `json:"Line"`
}

// pullQuickBooks reads the AR leg (Invoice), the GL leg (JournalEntry, one
// entity per line), and the bank legs (Purchase and Deposit).
func (s *Service) pullQuickBooks(ctx context.Context, cfg ProviderConfig, accountID, accessToken string) ([]entityRecord, error) {
	var out []entityRecord

	invoices, err := s.qboRecords(ctx, cfg, accountID, accessToken, "SELECT * FROM Invoice")
	if err != nil {
		return nil, err
	}
	for _, rec := range invoices["Invoice"] {
		cents, cerr := decimalToCents(rec.TotalAmt)
		if cerr != nil {
			return nil, fmt.Errorf("invoice %s: %w", rec.Id, cerr)
		}
		out = append(out, entityRecord{
			ExternalRef:       "qbo-invoice-" + rec.Id,
			EntityType:        "invoice_line_item",
			AmountCents:       cents,
			TransactionDate:   rec.TxnDate,
			Counterparty:      rec.CustomerRef.Name,
			Description:       "QBO Invoice " + rec.Id,
			ProviderUpdatedAt: rec.MetaData.LastUpdatedTime,
		})
	}

	entries, err := s.qboRecords(ctx, cfg, accountID, accessToken, "SELECT * FROM JournalEntry")
	if err != nil {
		return nil, err
	}
	for _, rec := range entries["JournalEntry"] {
		for i, line := range rec.Line {
			cents, cerr := decimalToCents(line.Amount)
			if cerr != nil {
				return nil, fmt.Errorf("journal entry %s line %d: %w", rec.Id, i, cerr)
			}
			out = append(out, entityRecord{
				ExternalRef:       fmt.Sprintf("qbo-je-%s-line-%d", rec.Id, i),
				EntityType:        "gl_entry",
				AmountCents:       cents,
				TransactionDate:   rec.TxnDate,
				Description:       line.Description,
				AccountCode:       line.JournalEntryLineDetail.AccountRef.Name,
				DebitOrCredit:     strings.ToLower(line.JournalEntryLineDetail.PostingType),
				ProviderUpdatedAt: rec.MetaData.LastUpdatedTime,
			})
		}
	}

	for _, q := range []string{"SELECT * FROM Purchase", "SELECT * FROM Deposit"} {
		records, err := s.qboRecords(ctx, cfg, accountID, accessToken, q)
		if err != nil {
			return nil, err
		}
		for key, list := range records {
			for _, rec := range list {
				cents, cerr := decimalToCents(rec.TotalAmt)
				if cerr != nil {
					return nil, fmt.Errorf("%s %s: %w", key, rec.Id, cerr)
				}
				out = append(out, entityRecord{
					ExternalRef:       "qbo-" + strings.ToLower(key) + "-" + rec.Id,
					EntityType:        "bank_transaction",
					AmountCents:       cents,
					TransactionDate:   rec.TxnDate,
					Counterparty:      firstNonEmpty(rec.CustomerRef.Name, rec.VendorRef.Name),
					Description:       "QBO " + key + " " + rec.Id,
					ProviderUpdatedAt: rec.MetaData.LastUpdatedTime,
				})
			}
		}
	}
	return out, nil
}

// qboRecords runs one query and returns the records keyed by the entity name
// QBO echoes back ("Invoice", "JournalEntry", …).
func (s *Service) qboRecords(ctx context.Context, cfg ProviderConfig, accountID, accessToken, query string) (map[string][]qboRecord, error) {
	endpoint := cfg.APIBase + "/" + accountID + "/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("query endpoint returned " + strconv.Itoa(resp.StatusCode) + ": " + truncate(body, 200))
	}
	var out struct {
		QueryResponse map[string][]qboRecord `json:"QueryResponse"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("query response not JSON")
	}
	return out.QueryResponse, nil
}

// pullXero reads the AR leg (Invoices), the bank leg (BankTransactions, one
// entity per transaction), and the GL leg (ManualJournals, one entity per
// line).
func (s *Service) pullXero(ctx context.Context, cfg ProviderConfig, accessToken string) ([]entityRecord, error) {
	var out []entityRecord

	var invoices []struct {
		InvoiceID      string `json:"InvoiceID"`
		Total          string `json:"Total"`
		Date           string `json:"Date"`
		UpdatedDateUTC string `json:"UpdatedDateUTC"`
		Contact        struct {
			Name string `json:"Name"`
		} `json:"Contact"`
	}
	if err := s.xeroGet(ctx, cfg, accessToken, "/api.xro/2.0/Invoices", &invoices); err != nil {
		return nil, err
	}
	for _, inv := range invoices {
		cents, cerr := decimalToCents(inv.Total)
		if cerr != nil {
			return nil, fmt.Errorf("invoice %s: %w", inv.InvoiceID, cerr)
		}
		out = append(out, entityRecord{
			ExternalRef:       "xero-invoice-" + inv.InvoiceID,
			EntityType:        "invoice_line_item",
			AmountCents:       cents,
			TransactionDate:   xeroDate(inv.Date),
			Counterparty:      inv.Contact.Name,
			Description:       "Xero Invoice " + inv.InvoiceID,
			ProviderUpdatedAt: inv.UpdatedDateUTC,
		})
	}

	var bank []struct {
		BankTransactionID string `json:"BankTransactionID"`
		Total             string `json:"Total"`
		Date              string `json:"Date"`
		UpdatedDateUTC    string `json:"UpdatedDateUTC"`
		BankAccount       struct {
			Name string `json:"Name"`
		} `json:"BankAccount"`
	}
	if err := s.xeroGet(ctx, cfg, accessToken, "/api.xro/2.0/BankTransactions", &bank); err != nil {
		return nil, err
	}
	for _, bt := range bank {
		cents, cerr := decimalToCents(bt.Total)
		if cerr != nil {
			return nil, fmt.Errorf("bank transaction %s: %w", bt.BankTransactionID, cerr)
		}
		out = append(out, entityRecord{
			ExternalRef:       "xero-bank-" + bt.BankTransactionID,
			EntityType:        "bank_transaction",
			AmountCents:       cents,
			TransactionDate:   xeroDate(bt.Date),
			Description:       "Xero Bank " + bt.BankAccount.Name,
			ProviderUpdatedAt: bt.UpdatedDateUTC,
		})
	}

	var journals []struct {
		ManualJournalID string `json:"ManualJournalID"`
		Date            string `json:"Date"`
		UpdatedDateUTC  string `json:"UpdatedDateUTC"`
		Narration       string `json:"Narration"`
		JournalLines    []struct {
			LineAmount  string `json:"LineAmount"`
			AccountCode string `json:"AccountCode"`
			Description string `json:"Description"`
		} `json:"JournalLines"`
	}
	if err := s.xeroGet(ctx, cfg, accessToken, "/api.xro/2.0/ManualJournals", &journals); err != nil {
		return nil, err
	}
	for _, j := range journals {
		for i, line := range j.JournalLines {
			cents, cerr := decimalToCents(line.LineAmount)
			if cerr != nil {
				return nil, fmt.Errorf("manual journal %s line %d: %w", j.ManualJournalID, i, cerr)
			}
			out = append(out, entityRecord{
				ExternalRef:       fmt.Sprintf("xero-mj-%s-line-%d", j.ManualJournalID, i),
				EntityType:        "gl_entry",
				AmountCents:       cents,
				TransactionDate:   xeroDate(j.Date),
				Description:       firstNonEmpty(line.Description, j.Narration),
				AccountCode:       line.AccountCode,
				ProviderUpdatedAt: j.UpdatedDateUTC,
			})
		}
	}
	return out, nil
}

// xeroGet is the shared Xero REST call: the auth header, the JSON decode
// into out, the error surface with the endpoint named.
func (s *Service) xeroGet(ctx context.Context, cfg ProviderConfig, accessToken, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", cfg.APIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("xero endpoint " + path + " returned " + strconv.Itoa(resp.StatusCode) + ": " + truncate(body, 200))
	}
	return json.Unmarshal(body, out)
}

// xeroDate normalises Xero's date forms ("/Date(1705276800000+0000)/" or
// "2024-01-15T00:00:00") to the bare YYYY-MM-DD the entity's transaction_date
// DATE column and NULLIF($n,”)::date expect.
func xeroDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/Date(") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "/Date("), ")/")
		if i := strings.Index(inner, "+"); i > 0 {
			inner = inner[:i]
		} else if i := strings.LastIndex(inner, "-"); i > 0 {
			inner = inner[:i]
		}
		if ms, err := strconv.ParseInt(inner, 10, 64); err == nil {
			return time.UnixMilli(ms).UTC().Format("2006-01-02")
		}
		return ""
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
