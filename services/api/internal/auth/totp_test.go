package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// testSecret is a fixed base32 TOTP secret (RFC 4648, no padding) so every case
// below is deterministic. 16 base32 chars = 80 bits, the RFC 4226 minimum.
const testSecret = "JBSWY3DPEHPK3PXP"

// rfc6238Code computes a 6-digit TOTP from first principles: HMAC-SHA1 over the
// big-endian 30-second counter, dynamic truncation per RFC 4226 §5.3, mod 10^6.
//
// It exists instead of calling the otp library's own generator so that the
// "valid code is accepted" assertion is a cross-check between two independent
// implementations of the spec. If this and totp.Validate agreed only because
// both came from the same package, the test would prove nothing about
// correctness — only self-consistency.
func rfc6238Code(t *testing.T, secretB32 string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.TrimRight(secretB32, "=")))
	if err != nil {
		t.Fatalf("decode base32 secret: %v", err)
	}

	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.UTC().Unix())/30)

	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	return fmt.Sprintf("%06d", value%1000000)
}

func TestRFC6238HelperAgreesWithLibrary(t *testing.T) {
	// Positive control for the helper itself. Every other test that asserts
	// "valid code accepted" depends on this, so if the two implementations ever
	// disagree, this test names the reason rather than letting a downstream case
	// fail for an unrelated-looking reason.
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	code := rfc6238Code(t, testSecret, now)
	if !totp.Validate(code, testSecret) {
		t.Fatalf("hand-rolled RFC 6238 code %q rejected by totp.Validate — the two implementations disagree", code)
	}
}

func TestNormalizeTOTPCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"123456", "123456"},
		{"123 456", "123456"},
		{"123-456", "123456"},
		{"  123456\n", "123456"},
		{"", ""},
		{"   ", ""},
		{"abcdef", ""},
		{"12a34b56", "123456"},
		{"1234567", "1234567"}, // length is judged by the caller, not here
	}
	for _, c := range cases {
		if got := NormalizeTOTPCode(c.in); got != c.want {
			t.Errorf("NormalizeTOTPCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCheckSecondFactor(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	valid := rfc6238Code(t, testSecret, now)
	stale := rfc6238Code(t, testSecret, now.Add(-5*time.Minute))

	// A code that is definitely wrong for `now`, whatever `valid` happens to be.
	wrong := "000000"
	if wrong == valid {
		wrong = "111111"
	}

	cases := []struct {
		name      string
		state     SecondFactorState
		submitted string
		want      error
	}{
		{
			name:      "not enrolled: no code needed",
			state:     SecondFactorState{},
			submitted: "",
			want:      nil,
		},
		{
			name:      "not enrolled: a stray code is ignored, not an error",
			state:     SecondFactorState{},
			submitted: "999999",
			want:      nil,
		},
		{
			name:      "enrolled: missing code fails closed",
			state:     SecondFactorState{Secret: testSecret},
			submitted: "",
			want:      ErrTOTPRequired,
		},
		{
			name:      "enrolled: whitespace-only code is missing, not invalid",
			state:     SecondFactorState{Secret: testSecret},
			submitted: "   ",
			want:      ErrTOTPRequired,
		},
		{
			name:      "enrolled: five digits rejected before the HMAC",
			state:     SecondFactorState{Secret: testSecret},
			submitted: "12345",
			want:      ErrTOTPInvalid,
		},
		{
			name:      "enrolled: seven digits rejected",
			state:     SecondFactorState{Secret: testSecret},
			submitted: "1234567",
			want:      ErrTOTPInvalid,
		},
		{
			name:      "enrolled: wrong code rejected",
			state:     SecondFactorState{Secret: testSecret},
			submitted: wrong,
			want:      ErrTOTPInvalid,
		},
		{
			name:      "enrolled: correct code accepted",
			state:     SecondFactorState{Secret: testSecret},
			submitted: valid,
			want:      nil,
		},
		{
			name:      "enrolled: correct code accepted with separators",
			state:     SecondFactorState{Secret: testSecret},
			submitted: valid[:3] + " " + valid[3:],
			want:      nil,
		},
		{
			name:      "enrolled: code from five minutes ago rejected",
			state:     SecondFactorState{Secret: testSecret},
			submitted: stale,
			want:      ErrTOTPInvalid,
		},
		{
			name: "replay: code already spent this instant",
			state: SecondFactorState{
				Secret:     testSecret,
				LastCode:   valid,
				LastUsedAt: now,
			},
			submitted: valid,
			want:      ErrTOTPReplay,
		},
		{
			name: "replay: still spent at 119s, inside the window",
			state: SecondFactorState{
				Secret:     testSecret,
				LastCode:   valid,
				LastUsedAt: now.Add(-119 * time.Second),
			},
			submitted: valid,
			want:      ErrTOTPReplay,
		},
		{
			name: "replay: released at 121s, past the window",
			state: SecondFactorState{
				Secret:     testSecret,
				LastCode:   valid,
				LastUsedAt: now.Add(-121 * time.Second),
			},
			submitted: valid,
			want:      nil,
		},
		{
			name: "replay: a future LastUsedAt keeps the code spent (fail closed under clock skew)",
			state: SecondFactorState{
				Secret:     testSecret,
				LastCode:   valid,
				LastUsedAt: now.Add(10 * time.Minute),
			},
			submitted: valid,
			want:      ErrTOTPReplay,
		},
		{
			name: "replay memory does not block a different valid code",
			state: SecondFactorState{
				Secret:     testSecret,
				LastCode:   stale,
				LastUsedAt: now.Add(-30 * time.Second),
			},
			submitted: valid,
			want:      nil,
		},
		{
			name: "replay memory with a zero timestamp is ignored",
			state: SecondFactorState{
				Secret:   testSecret,
				LastCode: valid,
			},
			submitted: valid,
			want:      nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CheckSecondFactor(c.state, c.submitted, now)
			if !errors.Is(got, c.want) {
				t.Fatalf("CheckSecondFactor = %v, want %v", got, c.want)
			}
		})
	}
}

// TestLoginRequestDecodesTOTPField pins the wire name. Both shipped clients send
// `totp_code` (apps/web/app/(auth)/login/page.tsx, apps/mobile LoginScreen), and
// the server had no such field until 2026-09-04 — the value was decoded into
// nothing and the second factor was never consulted. A rename on either side
// would silently restore that, so the exact JSON key is asserted here rather
// than left to review.
func TestLoginRequestDecodesTOTPField(t *testing.T) {
	body := `{"email":"a@b.com","password":"pw","totp_code":"123456"}`
	var req loginRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.TOTPCode != "123456" {
		t.Fatalf("TOTPCode = %q, want %q — the JSON tag no longer matches what the clients send", req.TOTPCode, "123456")
	}
}

// TestSecondFactorZeroValueMeansNotEnrolled guards the one assumption that makes
// the whole feature safe to ship to existing accounts: every user row created
// before this change has totp_secret NULL, which scans into an empty string, and
// an empty Secret must mean "no factor" rather than "factor with an empty
// secret". If this ever inverted, every existing user would be locked out.
func TestSecondFactorZeroValueMeansNotEnrolled(t *testing.T) {
	if err := CheckSecondFactor(SecondFactorState{}, "", time.Now()); err != nil {
		t.Fatalf("zero state must not require a code, got %v", err)
	}
}
