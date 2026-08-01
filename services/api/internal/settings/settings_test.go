package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// Known-answer vector for HMAC-SHA256 (RFC 4231 test case 1).
func TestSignHMACKnownAnswer(t *testing.T) {
	secret := "\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b\x0b"
	body := "Hi There"
	want := "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
	if got := SignHMAC(secret, body); got != want {
		t.Errorf("SignHMAC mismatch: got %s want %s", got, want)
	}
}

func TestSignHMACDeterministicAndSensitive(t *testing.T) {
	a := SignHMAC("secret-1", `{"event":"ping"}`)
	b := SignHMAC("secret-1", `{"event":"ping"}`)
	if a != b {
		t.Fatal("same secret+body must produce same signature")
	}
	c := SignHMAC("secret-2", `{"event":"ping"}`)
	if a == c {
		t.Fatal("different secret must produce different signature")
	}
	d := SignHMAC("secret-1", `{"event":"pong"}`)
	if a == d {
		t.Fatal("different body must produce different signature")
	}
}

func TestValidateAPIKeyFormat(t *testing.T) {
	valid := []string{
		"aiaud_" + strings.Repeat("a", 32),
		"aiaud_" + strings.Repeat("0", 32),
		"aiaud_" + strings.Repeat("f", 32),
	}
	for _, k := range valid {
		if !ValidateAPIKeyFormat(k) {
			t.Errorf("expected %q to be a valid API key format", k)
		}
	}
	invalid := []string{
		"", "aiaud_", "aiaud_" + strings.Repeat("a", 31), // too short
		"aiaud_" + strings.Repeat("a", 33), // too long
		"x" + strings.Repeat("a", 32),      // wrong prefix
		"aiaud_" + strings.Repeat("g", 32), // non-hex
		"AIaud_" + strings.Repeat("a", 32), // case-sensitive prefix
		"aiaud_" + strings.Repeat("a", 32) + "\n",
	}
	for _, k := range invalid {
		if ValidateAPIKeyFormat(k) {
			t.Errorf("expected %q to be an invalid API key format", k)
		}
	}
}

func TestGenerateAPIKey(t *testing.T) {
	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	if !ValidateAPIKeyFormat(key) {
		t.Fatalf("generated key %q fails format validation", key)
	}
	k2, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey failed: %v", err)
	}
	if key == k2 {
		t.Fatal("two generated keys must not collide")
	}
}

func TestHashKey(t *testing.T) {
	sum := sha256.Sum256([]byte("aiaud_test"))
	want := hex.EncodeToString(sum[:])
	if got := HashKey("aiaud_test"); got != want {
		t.Errorf("HashKey mismatch: got %s want %s", got, want)
	}
	// The hash must never reveal the raw key.
	if strings.Contains(HashKey("aiaud_test"), "aiaud_test") {
		t.Fatal("hash must not contain the raw key")
	}
}

func TestDefaultReconcilable(t *testing.T) {
	cases := []struct {
		accountType string
		provided    *bool
		want        bool
	}{
		{"equity", nil, false}, // equity defaults to non-reconcilable
		{"asset", nil, true},   // everything else defaults to reconcilable
		{"liability", nil, true},
		{"revenue", nil, true},
		{"expense", nil, true},
		{"equity", boolPtr(true), true}, // explicit override wins
		{"equity", boolPtr(false), false},
		{"asset", boolPtr(false), false}, // explicit false on asset honored
	}
	for _, c := range cases {
		if got := defaultReconcilable(c.accountType, c.provided); got != c.want {
			t.Errorf("defaultReconcilable(%q, %v) = %v, want %v", c.accountType, c.provided, got, c.want)
		}
	}
}

func TestValidAccountType(t *testing.T) {
	for _, ok := range []string{"asset", "liability", "equity", "revenue", "expense"} {
		if !validAccountType(ok) {
			t.Errorf("expected %q to be a valid account type", ok)
		}
	}
	for _, bad := range []string{"", "asset ", "Asset", "contra_asset", "income"} {
		if validAccountType(bad) {
			t.Errorf("expected %q to be an invalid account type", bad)
		}
	}
}

func TestSeedTemplatesWellFormed(t *testing.T) {
	if len(coaTemplateSeeds) != 3 {
		t.Fatalf("expected 3 seed templates, got %d", len(coaTemplateSeeds))
	}
	seenCodes := map[string]map[string]bool{}
	for _, s := range coaTemplateSeeds {
		if s.Name == "" {
			t.Errorf("template %q has empty name", s.Name)
		}
		if s.Industry == "" {
			t.Errorf("template %q has empty industry", s.Name)
		}
		if len(s.Accounts) != 15 {
			t.Errorf("template %q has %d accounts, want 15", s.Name, len(s.Accounts))
		}
		seenCodes[s.Name] = map[string]bool{}
		for _, a := range s.Accounts {
			if a.AccountCode == "" || a.AccountName == "" {
				t.Errorf("template %q has account with empty code/name", s.Name)
			}
			if seenCodes[s.Name][a.AccountCode] {
				t.Errorf("template %q duplicates account code %q", s.Name, a.AccountCode)
			}
			seenCodes[s.Name][a.AccountCode] = true
			if !validAccountType(a.AccountType) {
				t.Errorf("template %q account %q has invalid type %q", s.Name, a.AccountCode, a.AccountType)
			}
		}
	}
}

func boolPtr(b bool) *bool { return &b }
