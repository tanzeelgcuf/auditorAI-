// Package email — templates for transactional mail (doc 12 §4).
package email

import (
	"fmt"
)

// Template names
const (
	VerifyEmailTemplate = "verify_email"
	ResetPasswordTemplate = "reset_password"
	StaffInviteTemplate = "staff_invite"
)

// Data holds the dynamic fields for a given template.
type TemplateData struct {
	VerifyURL       string // for verify_email
	ResetURL        string // for reset_password
	InviteURL       string // for staff_invite
	FirmName        string
	UserName        string // optional, email local-part used if empty
	BookName        string // for staff_invite
	Role            string // for staff_invite (e.g., "staff", "firm_admin")
	ExpirationHours int
}

func verifyHTML(d TemplateData) string {
	return fmt.Sprintf(`<!doctype html>
<html>
  <body style="font-family: -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif; line-height:1.5; color:#111;">
    <div style="max-width:600px;margin:0 auto;padding:24px;">
      <h1 style="margin:0 0 16px;">Welcome to AI Auditor</h1>
      <p>Hi%s,</p>
      <p>Thanks for signing up. Verify your email to activate your account:</p>
      <p style="margin:24px 0;">
        <a href="%s" style="display:inline-block;background:#2563eb;color:#fff;padding:12px 24px;border-radius:6px;text-decoration:none;">Verify Email</a>
      </p>
      <p style="color:#666;font-size:0.9rem;">This link expires in %d hours. If you didn't create an account, you can ignore this email.</p>
      <hr style="border:none;border-top:1px solid #eee;margin:24px 0;">
      <p style="color:#999;font-size:0.8rem;">AI Auditor · %s</p>
    </div>
  </body>
</html>`, userName(d.UserName, d.FirmName), d.VerifyURL, d.ExpirationHours, d.FirmName)
}

func verifyText(d TemplateData) string {
	return fmt.Sprintf(`Welcome to AI Auditor

Hi%s,

Thanks for signing up. Verify your email to activate your account:

%s

This link expires in %d hours. If you didn't create an account, you can ignore this email.

—
AI Auditor · %s`, userName(d.UserName, d.FirmName), d.VerifyURL, d.ExpirationHours, d.FirmName)
}

func resetHTML(d TemplateData) string {
	return fmt.Sprintf(`<!doctype html>
<html>
  <body style="font-family: -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif; line-height:1.5; color:#111;">
    <div style="max-width:600px;margin:0 auto;padding:24px;">
      <h1 style="margin:0 0 16px;">Reset Your Password</h1>
      <p>Hi%s,</p>
      <p>You requested a password reset. Click below to set a new password:</p>
      <p style="margin:24px 0;">
        <a href="%s" style="display:inline-block;background:#2563eb;color:#fff;padding:12px 24px;border-radius:6px;text-decoration:none;">Reset Password</a>
      </p>
      <p style="color:#666;font-size:0.9rem;">This link expires in %d hours. If you didn't request this, you can ignore this email.</p>
      <hr style="border:none;border-top:1px solid #eee;margin:24px 0;">
      <p style="color:#999;font-size:0.8rem;">AI Auditor · %s</p>
    </div>
  </body>
</html>`, userName(d.UserName, d.FirmName), d.ResetURL, d.ExpirationHours, d.FirmName)
}

func resetText(d TemplateData) string {
	return fmt.Sprintf(`Reset Your Password

Hi%s,

You requested a password reset. Click below to set a new password:

%s

This link expires in %d hours. If you didn't request this, you can ignore this email.

—
AI Auditor · %s`, userName(d.UserName, d.FirmName), d.ResetURL, d.ExpirationHours, d.FirmName)
}

func inviteHTML(d TemplateData) string {
	return fmt.Sprintf(`<!doctype html>
<html>
  <body style="font-family: -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif; line-height:1.5; color:#111;">
    <div style="max-width:600px;margin:0 auto;padding:24px;">
      <h1 style="margin:0 0 16px;">You're Invited to %s</h1>
      <p>Hi%s,</p>
      <p>You've been invited to join <strong>%s</strong> as a <strong>%s</strong>.</p>
      <p style="margin:24px 0;">
        <a href="%s" style="display:inline-block;background:#2563eb;color:#fff;padding:12px 24px;border-radius:6px;text-decoration:none;">Accept Invitation</a>
      </p>
      <p style="color:#666;font-size:0.9rem;">This invitation expires in %d hours.</p>
      <hr style="border:none;border-top:1px solid #eee;margin:24px 0;">
      <p style="color:#999;font-size:0.8rem;">AI Auditor · %s</p>
    </div>
  </body>
</html>`, d.FirmName, userName(d.UserName, ""), d.BookName, d.Role, d.InviteURL, d.ExpirationHours, d.FirmName)
}

func inviteText(d TemplateData) string {
	return fmt.Sprintf(`You're Invited to %s

Hi%s,

You've been invited to join %s as a %s.

%s

This invitation expires in %d hours.

—
AI Auditor · %s`, d.FirmName, userName(d.UserName, ""), d.BookName, d.Role, d.InviteURL, d.ExpirationHours, d.FirmName)
}

func userName(fallback, firmName string) string {
	if fallback != "" {
		return " " + fallback
	}
	if firmName != "" {
		return ""
	}
	return ""
}

// Render renders a template by name with data, returning (html, text).
func Render(name string, d TemplateData) (html string, text string) {
	switch name {
	case VerifyEmailTemplate:
		return verifyHTML(d), verifyText(d)
	case ResetPasswordTemplate:
		return resetHTML(d), resetText(d)
	case StaffInviteTemplate:
		return inviteHTML(d), inviteText(d)
	default:
		return "", ""
	}
}