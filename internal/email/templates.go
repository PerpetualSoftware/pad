package email

import (
	"context"
	"fmt"
	"html"
)

// SendInvitation sends a workspace invitation email.
// If unsubscribeURL is non-empty, an unsubscribe link is included in the footer.
func (s *Sender) SendInvitation(ctx context.Context, to, inviterName, workspaceName, joinURL, unsubscribeURL string) error {
	subject := fmt.Sprintf("%s invited you to %s on Pad", inviterName, workspaceName)

	unsubHTML := ""
	unsubPlain := ""
	if unsubscribeURL != "" {
		unsubHTML = fmt.Sprintf(` <a href="%s" style="color: #999; text-decoration: underline;">Unsubscribe</a> from future emails.`, unsubscribeURL)
		unsubPlain = fmt.Sprintf("\nUnsubscribe from future emails: %s", unsubscribeURL)
	}

	htmlBody := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 40px 20px;">
  <div style="margin-bottom: 32px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
  <p style="font-size: 16px; line-height: 1.5;">
    <strong>%s</strong> has invited you to join <strong>%s</strong> on Pad.
  </p>
  <p style="margin: 32px 0;">
    <a href="%s" style="display: inline-block; padding: 12px 28px; background: #2563eb; color: #fff; text-decoration: none; border-radius: 6px; font-size: 15px; font-weight: 500;">
      Accept Invitation
    </a>
  </p>
  <p style="font-size: 13px; color: #666; line-height: 1.5;">
    Or copy this link: <a href="%s" style="color: #2563eb;">%s</a>
  </p>
  <hr style="border: none; border-top: 1px solid #e5e5e5; margin: 32px 0;" />
  <p style="font-size: 12px; color: #999;">
    You received this email because someone invited you to a Pad workspace.
    If you didn't expect this, you can safely ignore it.%s
  </p>
</body>
</html>`,
		html.EscapeString(inviterName),
		html.EscapeString(workspaceName),
		joinURL,
		joinURL,
		joinURL,
		unsubHTML,
	)

	plainBody := fmt.Sprintf(`%s has invited you to join %s on Pad.

Accept the invitation: %s

---
You received this email because someone invited you to a Pad workspace.
If you didn't expect this, you can safely ignore it.%s`,
		inviterName, workspaceName, joinURL, unsubPlain,
	)

	// Use inviter's name as the sender display name: "Dave via Pad"
	fromName := fmt.Sprintf("%s via Pad", inviterName)
	return s.SendAs(ctx, fromName, to, "", subject, htmlBody, plainBody)
}

// SendWelcome sends a welcome email after registration.
// If unsubscribeURL is non-empty, an unsubscribe link is included in the footer.
func (s *Sender) SendWelcome(ctx context.Context, to, name, unsubscribeURL string) error {
	subject := "Welcome to Pad"

	unsubHTML := ""
	unsubPlain := ""
	if unsubscribeURL != "" {
		unsubHTML = fmt.Sprintf(` <a href="%s" style="color: #999; text-decoration: underline;">Unsubscribe</a> from future emails.`, unsubscribeURL)
		unsubPlain = fmt.Sprintf("\nUnsubscribe from future emails: %s", unsubscribeURL)
	}

	htmlBody := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 40px 20px;">
  <div style="margin-bottom: 32px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
  <p style="font-size: 16px; line-height: 1.5;">
    Hi %s, welcome to Pad!
  </p>
  <p style="font-size: 15px; line-height: 1.5; color: #444;">
    Your account has been created. You can now create workspaces, manage projects, and collaborate with your team.
  </p>
  <p style="margin: 32px 0;">
    <a href="%s" style="display: inline-block; padding: 12px 28px; background: #2563eb; color: #fff; text-decoration: none; border-radius: 6px; font-size: 15px; font-weight: 500;">
      Open Pad
    </a>
  </p>
  <hr style="border: none; border-top: 1px solid #e5e5e5; margin: 32px 0;" />
  <p style="font-size: 12px; color: #999;">
    You received this because an account was created with this email address.%s
  </p>
</body>
</html>`,
		html.EscapeString(name),
		s.baseURL,
		unsubHTML,
	)

	plainBody := fmt.Sprintf(`Hi %s, welcome to Pad!

Your account has been created. You can now create workspaces, manage projects, and collaborate with your team.

Open Pad: %s%s`,
		name, s.baseURL, unsubPlain,
	)

	return s.Send(ctx, to, name, subject, htmlBody, plainBody)
}

// SendPasswordReset sends a password reset email with a token link.
// This is a placeholder — the password reset flow (IDEA-81) will call this
// once the reset token endpoint exists.
func (s *Sender) SendPasswordReset(ctx context.Context, to, name, resetURL string) error {
	subject := "Reset your Pad password"

	htmlBody := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 40px 20px;">
  <div style="margin-bottom: 32px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
  <p style="font-size: 16px; line-height: 1.5;">
    Hi %s, we received a request to reset your password.
  </p>
  <p style="margin: 32px 0;">
    <a href="%s" style="display: inline-block; padding: 12px 28px; background: #2563eb; color: #fff; text-decoration: none; border-radius: 6px; font-size: 15px; font-weight: 500;">
      Reset Password
    </a>
  </p>
  <p style="font-size: 13px; color: #666; line-height: 1.5;">
    This link expires in 1 hour. If you didn't request a password reset, you can safely ignore this email.
  </p>
  <hr style="border: none; border-top: 1px solid #e5e5e5; margin: 32px 0;" />
  <p style="font-size: 12px; color: #999;">
    You received this because a password reset was requested for your Pad account.
  </p>
</body>
</html>`,
		html.EscapeString(name),
		resetURL,
	)

	plainBody := fmt.Sprintf(`Hi %s, we received a request to reset your password.

Reset your password: %s

This link expires in 1 hour. If you didn't request a password reset, you can safely ignore this email.`,
		name, resetURL,
	)

	return s.Send(ctx, to, name, subject, htmlBody, plainBody)
}

// SendTest sends a test email to verify the email configuration.
func (s *Sender) SendTest(ctx context.Context, to string) error {
	subject := "Pad — Test Email"

	htmlBody := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 560px; margin: 0 auto; padding: 40px 20px;">
  <div style="margin-bottom: 32px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
  <p style="font-size: 16px; line-height: 1.5;">
    This is a test email from your Pad instance. If you're reading this, email delivery is working correctly!
  </p>
  <hr style="border: none; border-top: 1px solid #e5e5e5; margin: 32px 0;" />
  <p style="font-size: 12px; color: #999;">
    Sent from Pad platform settings.
  </p>
</body>
</html>`

	plainBody := "This is a test email from your Pad instance. If you're reading this, email delivery is working correctly!"

	return s.Send(ctx, to, "", subject, htmlBody, plainBody)
}
