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
	cloudMode := s.CloudMode()

	unsubHTML := ""
	unsubPlain := ""
	if unsubscribeURL != "" {
		unsubHTML = fmt.Sprintf(` <a href="%s" style="color: #999; text-decoration: underline;">Unsubscribe</a> from future emails.`, unsubscribeURL)
		unsubPlain = fmt.Sprintf("\nUnsubscribe from future emails: %s", unsubscribeURL)
	}

	body := fmt.Sprintf(`  <p style="font-size: 16px; line-height: 1.5;">
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
`,
		html.EscapeString(inviterName),
		html.EscapeString(workspaceName),
		joinURL,
		joinURL,
		joinURL,
	)

	footerNoteHTML := fmt.Sprintf(`You received this email because someone invited you to a Pad workspace.
    If you didn't expect this, you can safely ignore it.%s`, unsubHTML)

	htmlBody := buildHTMLShell(body, footerNoteHTML, cloudMode)

	plainBody := buildPlainShell(
		fmt.Sprintf(`%s has invited you to join %s on Pad.

Accept the invitation: %s`, inviterName, workspaceName, joinURL),
		fmt.Sprintf(`You received this email because someone invited you to a Pad workspace.
If you didn't expect this, you can safely ignore it.%s`, unsubPlain),
		cloudMode,
	)

	// Use inviter's name as the sender display name: "Dave via Pad"
	fromName := fmt.Sprintf("%s via Pad", inviterName)
	return s.SendAs(ctx, fromName, to, "", subject, htmlBody, plainBody)
}

// SendWelcome sends a welcome email after registration.
// If unsubscribeURL is non-empty, an unsubscribe link is included in the footer.
func (s *Sender) SendWelcome(ctx context.Context, to, name, unsubscribeURL string) error {
	subject := "Welcome to Pad"
	cloudMode := s.CloudMode()

	unsubHTML := ""
	unsubPlain := ""
	if unsubscribeURL != "" {
		unsubHTML = fmt.Sprintf(` <a href="%s" style="color: #999; text-decoration: underline;">Unsubscribe</a> from future emails.`, unsubscribeURL)
		unsubPlain = fmt.Sprintf("\nUnsubscribe from future emails: %s", unsubscribeURL)
	}

	body := fmt.Sprintf(`  <p style="font-size: 16px; line-height: 1.5;">
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
`,
		html.EscapeString(name),
		s.baseURL,
	)

	footerNoteHTML := fmt.Sprintf("You received this because an account was created with this email address.%s", unsubHTML)

	htmlBody := buildHTMLShell(body, footerNoteHTML, cloudMode)

	plainBody := buildPlainShell(
		fmt.Sprintf(`Hi %s, welcome to Pad!

Your account has been created. You can now create workspaces, manage projects, and collaborate with your team.

Open Pad: %s`, name, s.baseURL),
		fmt.Sprintf("You received this because an account was created with this email address.%s", unsubPlain),
		cloudMode,
	)

	return s.Send(ctx, to, name, subject, htmlBody, plainBody)
}

// SendPasswordReset sends a password reset email with a token link.
// This is a placeholder — the password reset flow (IDEA-81) will call this
// once the reset token endpoint exists.
func (s *Sender) SendPasswordReset(ctx context.Context, to, name, resetURL string) error {
	subject := "Reset your Pad password"
	cloudMode := s.CloudMode()

	body := fmt.Sprintf(`  <p style="font-size: 16px; line-height: 1.5;">
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
`,
		html.EscapeString(name),
		resetURL,
	)

	footerNoteHTML := "You received this because a password reset was requested for your Pad account."

	htmlBody := buildHTMLShell(body, footerNoteHTML, cloudMode)

	plainBody := buildPlainShell(
		fmt.Sprintf(`Hi %s, we received a request to reset your password.

Reset your password: %s

This link expires in 1 hour. If you didn't request a password reset, you can safely ignore this email.`, name, resetURL),
		"You received this because a password reset was requested for your Pad account.",
		cloudMode,
	)

	return s.Send(ctx, to, name, subject, htmlBody, plainBody)
}

// SendPaymentFailed notifies a user that a Stripe invoice attempt failed.
// Called by the sidecar (via POST /api/v1/admin/payment-failed) after it
// handles an invoice.payment_failed webhook. The email links to the
// billing portal so the user can update their card before Stripe's next
// dunning attempt. amountDisplay is a pre-formatted human-readable
// string like "$10.00" or empty to omit the amount line; nextRetryDisplay
// is the same for the retry date ("April 30, 2026" or empty).
func (s *Sender) SendPaymentFailed(ctx context.Context, to, name, amountDisplay, nextRetryDisplay, billingPortalURL string) error {
	subject := "Your Pad payment couldn't be processed"
	cloudMode := s.CloudMode()

	// Build optional lines conditionally so we don't ship empty paragraphs
	// when the webhook payload lacks amount or retry info (Stripe normally
	// includes both, but we avoid assuming it).
	amountLineHTML := ""
	amountLinePlain := ""
	if amountDisplay != "" {
		amountLineHTML = fmt.Sprintf(
			`<p style="font-size: 15px; line-height: 1.5; margin: 0 0 12px;">Amount: <strong>%s</strong></p>`,
			html.EscapeString(amountDisplay),
		)
		amountLinePlain = fmt.Sprintf("Amount: %s\n", amountDisplay)
	}

	retryLineHTML := ""
	retryLinePlain := ""
	if nextRetryDisplay != "" {
		retryLineHTML = fmt.Sprintf(
			`<p style="font-size: 15px; line-height: 1.5; margin: 0 0 24px;">Stripe will retry on <strong>%s</strong>. To avoid an interruption, update your card before then.</p>`,
			html.EscapeString(nextRetryDisplay),
		)
		retryLinePlain = fmt.Sprintf("Stripe will retry on %s. To avoid an interruption, update your card before then.\n\n", nextRetryDisplay)
	} else {
		retryLineHTML = `<p style="font-size: 15px; line-height: 1.5; margin: 0 0 24px;">Stripe will retry automatically over the next few days. To avoid an interruption, update your card before then.</p>`
		retryLinePlain = "Stripe will retry automatically over the next few days. To avoid an interruption, update your card before then.\n\n"
	}

	body := fmt.Sprintf(`  <p style="font-size: 16px; line-height: 1.5; margin: 0 0 16px;">
    Hi %s,
  </p>
  <p style="font-size: 16px; line-height: 1.5; margin: 0 0 16px;">
    We tried to charge your card for your Pad Pro subscription but the payment didn't go through.
  </p>
  %s
  %s
  <p style="margin: 32px 0;">
    <a href="%s" style="display: inline-block; padding: 12px 28px; background: #2563eb; color: #fff; text-decoration: none; border-radius: 6px; font-size: 15px; font-weight: 500;">
      Update payment method
    </a>
  </p>
  <p style="font-size: 13px; color: #666; line-height: 1.5;">
    If you meant to cancel, you can ignore this email — the subscription will cancel automatically after Stripe's retries are exhausted.
  </p>
`,
		html.EscapeString(name),
		amountLineHTML,
		retryLineHTML,
		html.EscapeString(billingPortalURL),
	)

	footerNoteHTML := "You received this because your Pad account has a Pro subscription with a failed payment. Replies go to support@getpad.dev."

	htmlBody := buildHTMLShell(body, footerNoteHTML, cloudMode)

	plainBody := buildPlainShell(
		fmt.Sprintf(`Hi %s,

We tried to charge your card for your Pad Pro subscription but the payment didn't go through.

%s%sUpdate your payment method: %s

If you meant to cancel, you can ignore this email — the subscription will cancel automatically after Stripe's retries are exhausted.`,
			name,
			amountLinePlain,
			retryLinePlain,
			billingPortalURL,
		),
		"You received this because your Pad account has a Pro subscription with a failed payment. Replies go to support@getpad.dev.",
		cloudMode,
	)

	return s.Send(ctx, to, name, subject, htmlBody, plainBody)
}

// SendTest sends a test email to verify the email configuration.
func (s *Sender) SendTest(ctx context.Context, to string) error {
	subject := "Pad — Test Email"
	cloudMode := s.CloudMode()

	body := `  <p style="font-size: 16px; line-height: 1.5;">
    This is a test email from your Pad instance. If you're reading this, email delivery is working correctly!
  </p>
`

	footerNoteHTML := "Sent from Pad platform settings."

	htmlBody := buildHTMLShell(body, footerNoteHTML, cloudMode)

	plainBody := buildPlainShell(
		"This is a test email from your Pad instance. If you're reading this, email delivery is working correctly!",
		"Sent from Pad platform settings.",
		cloudMode,
	)

	return s.Send(ctx, to, "", subject, htmlBody, plainBody)
}
