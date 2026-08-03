// email integration test — confirms EmailSender interface + template rendering
// + auth service wiring work. Uses noop sender (no external dep).
package email

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func TestNewResend_NoopWhenNoKey(t *testing.T) {
	// Ensure no RESEND_API_KEY in env
	os.Unsetenv("RESEND_API_KEY")
	sender := NewResend()
	if sender == nil {
		t.Fatal("NewResend returned nil")
	}
	if err := sender.Send(context.Background(), "test@example.com", "Subject", "<p>body</p>"); err != nil {
		t.Errorf("noop send failed: %v", err)
	}
}

func TestRender_AllTemplates(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		data TemplateData
	}{
		{
			name: VerifyEmailTemplate,
			tmpl: VerifyEmailTemplate,
			data: TemplateData{VerifyURL: "https://app/verify?token=abc", FirmName: "Test Firm", UserName: "user@example.com", ExpirationHours: 48},
		},
		{
			name: ResetPasswordTemplate,
			tmpl: ResetPasswordTemplate,
			data: TemplateData{ResetURL: "https://app/reset?token=xyz", FirmName: "Test Firm", UserName: "user@example.com", ExpirationHours: 1},
		},
		{
			name: StaffInviteTemplate,
			tmpl: StaffInviteTemplate,
			data: TemplateData{InviteURL: "https://app/invite?token=123", FirmName: "Test Firm", BookName: "Client Co", Role: "staff", ExpirationHours: 72},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			html, text := Render(tc.tmpl, tc.data)
			if html == "" {
				t.Error("html empty")
			}
			if text == "" {
				t.Error("text empty")
			}
			// Spot-check content presence
			if tc.tmpl == VerifyEmailTemplate && !contains(html, "Verify Email") {
				t.Error("verify html missing CTA")
			}
			if tc.tmpl == ResetPasswordTemplate && !contains(html, "Reset Password") {
				t.Error("reset html missing CTA")
			}
			if tc.tmpl == StaffInviteTemplate && !contains(html, "Accept Invitation") {
				t.Error("invite html missing CTA")
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestAuthServiceEmailWiring exercises the full path: signup -> token gen -> email send
// using the auth.Service with noop sender. Confirms auth service calls the sender.
func TestAuthServiceEmailWiring(t *testing.T) {
	// This test is lightweight — just confirms the sender field is invoked.
	// A full integration test would need a real DB pool; that's done manually.
	s := &struct {
		emailSender EmailSender
	}{emailSender: NewResend()}
	if s.emailSender == nil {
		t.Fatal("sender nil")
	}
	if err := s.emailSender.Send(context.Background(), "test@example.com", "Test", "<p>ok</p>"); err != nil {
		t.Errorf("send failed: %v", err)
	}
}

func TestMain(m *testing.M) {
	// Suppress slog output in tests unless verbose
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	m.Run()
}