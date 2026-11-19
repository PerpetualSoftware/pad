package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// `pad token` — mint, list, and revoke user-scoped API tokens from the
// CLI (#879 follow-up). Tokens are the identity PAD_TOKEN carries, and
// minting was web-only before this group, so a headless agent setup
// couldn't get its own identity without a browser.

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens for automation and agents",
		RunE:  unknownSubcommandRun,
		Long: `Manage user-scoped API tokens (the pad_ tokens PAD_TOKEN carries).

Tokens act as the user who minted them. The secret is shown exactly once,
at mint time — the server stores only a hash and cannot show it again.

Creating or rotating a token, or widening its access, needs a session
signed in within the last 10 minutes or a verified identity: sign in
('pad auth login'), then run the command within 10 minutes. With an older
session the command asks you to verify your identity first (a code from
your authenticator app or a recovery code, a code sent by email, or your
password) and then retries once. A script that signs in and then creates
a token straight away needs no prompt.`,
		Example: `  pad token create --name ci-agent
  pad token create --name cursor --expires-in 30
  pad token list
  pad token edit 7fde5e41-... --scopes read
  pad token rotate 7fde5e41-...
  pad token revoke 7fde5e41-...`,
	}
	cmd.AddCommand(
		tokenCreateCmd(),
		tokenListCmd(),
		tokenEditCmd(),
		tokenRotateCmd(),
		tokenRevokeCmd(),
	)
	return cmd
}

func tokenCreateCmd() *cobra.Command {
	var (
		nameFlag      string
		expiresInFlag int
		scopesFlag    string
	)

	cmd := &cobra.Command{
		Use:     "create --name <name>",
		Short:   "Mint a new API token (secret shown once)",
		Example: `  pad token create ci-deploy --scopes read --expires-in 90`,
		Long: `Mint a new API token. The secret is shown exactly once.

This needs a session signed in within the last 10 minutes: sign in
('pad auth login'), then create within 10 minutes. With an older session
you are asked to verify your identity first (an authenticator or recovery
code, an emailed code, or your password; a code or password may be piped
on stdin, an emailed code needs a terminal), then the create is retried
once. A call authenticated by a pad_ API token is refused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			scopes, err := encodeTokenScopes(scopesFlag)
			if err != nil {
				return err
			}

			client, _ := getClient()

			input := models.APITokenCreate{
				Name:      nameFlag,
				Scopes:    scopes,
				ExpiresIn: expiresInFlag,
			}

			var token *models.APITokenWithSecret
			err = retryAfterStepUp(client, bufio.NewReader(os.Stdin), cmd.ErrOrStderr(), cli.IsTerminal(), "no token was created", func() (err error) {
				token, err = client.CreateUserToken(input)
				return err
			})
			if err != nil {
				// Same structured marker as webhook create (TASK-788) so
				// callers can tell a plan limit from a server fault.
				if apiErr := cli.AsAPIError(err); apiErr != nil {
					if apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("token creation blocked: plan limit reached")
					}
				}
				return keyStepUpError(err, "no token was created")
			}

			if formatFlag == "json" {
				return cli.PrintJSON(token)
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Token created: %s\n", green.Sprint("✓"), token.Name)
			fmt.Printf("  ID:      %s\n", token.ID)
			fmt.Printf("  Prefix:  %s\n", token.Prefix)
			fmt.Printf("  Expires: %s\n", formatTokenExpiry(token.ExpiresAt))
			fmt.Println()
			fmt.Printf("  %s\n", color.New(color.Bold).Sprint(token.Token))
			fmt.Println()
			yellow := color.New(color.FgYellow)
			fmt.Printf("%s This token is shown only this once — store it now (e.g. in the agent's PAD_TOKEN).\n", yellow.Sprint("!"))
			return nil
		},
	}

	cmd.Flags().StringVar(&nameFlag, "name", "", "token name (required; shown in list and audit log)")
	cmd.Flags().IntVar(&expiresInFlag, "expires-in", 0, "expiry in days (0 = platform default)")
	cmd.Flags().StringVar(&scopesFlag, "scopes", "", "comma-separated scopes: *, read, write, pad:read, pad:write, pad:admin (default: full access)")
	_ = cmd.MarkFlagRequired("name")

	return cmd
}

func tokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List your API tokens (metadata only, never secrets)",
		Example: `  pad token list`,
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			tokens, err := client.ListUserTokens()
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(tokens)
			}

			if len(tokens) == 0 {
				fmt.Println("No API tokens. Mint one with: pad token create --name <name>")
				return nil
			}

			dim := color.New(color.Faint)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("ID"),
				dim.Sprint("NAME"),
				dim.Sprint("PREFIX"),
				dim.Sprint("CREATED"),
				dim.Sprint("LAST USED"),
				dim.Sprint("EXPIRES"),
			)
			for _, tok := range tokens {
				lastUsed := "never"
				if tok.LastUsedAt != nil {
					lastUsed = cli.RelativeTime(*tok.LastUsedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					tok.ID,
					tok.Name,
					tok.Prefix,
					cli.RelativeTime(tok.CreatedAt),
					lastUsed,
					formatTokenExpiry(tok.ExpiresAt),
				)
			}
			w.Flush()
			return nil
		},
	}
}

func tokenRotateCmd() *cobra.Command {
	var expiresInFlag int

	cmd := &cobra.Command{
		Use:     "rotate <token-id>",
		Short:   "Replace a token's secret (the old one stops working immediately)",
		Example: `  pad token rotate <token-id>`,
		Long: `Rotate an API token by its exact id (from 'pad token list'): the server
generates a new secret and the old one stops working with the same write —
there is no grace window, so anything still using the old secret fails on
its next call. Metadata (name, scopes) is preserved; last-used resets.

The new secret is shown exactly once. --expires-in sets a new expiry in
days; without it the original expiry is preserved.

Like create, rotation needs a login session (a call authenticated by a
pad_ API token is refused with 403 session_required) signed in within the
last 10 minutes: sign in ('pad auth login'), then rotate within 10
minutes. With an older session you are asked to verify your identity
first, then the rotation is retried once.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			var token *models.APITokenWithSecret
			err := retryAfterStepUp(client, bufio.NewReader(os.Stdin), cmd.ErrOrStderr(), cli.IsTerminal(), "the old secret still works", func() (err error) {
				token, err = client.RotateUserToken(args[0], expiresInFlag)
				return err
			})
			if err != nil {
				return keyStepUpError(err, "the old secret still works")
			}

			if formatFlag == "json" {
				return cli.PrintJSON(token)
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Token rotated: %s\n", green.Sprint("✓"), token.Name)
			fmt.Printf("  ID:      %s\n", token.ID)
			fmt.Printf("  Prefix:  %s\n", token.Prefix)
			fmt.Printf("  Expires: %s\n", formatTokenExpiry(token.ExpiresAt))
			fmt.Println()
			fmt.Printf("  %s\n", color.New(color.Bold).Sprint(token.Token))
			fmt.Println()
			yellow := color.New(color.FgYellow)
			fmt.Printf("%s This secret is shown only this once — store it now. The old secret stopped working immediately.\n", yellow.Sprint("!"))
			return nil
		},
	}

	cmd.Flags().IntVar(&expiresInFlag, "expires-in", 0, "new expiry in days (0 = keep the original expiry)")

	return cmd
}

func tokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "revoke <token-id>",
		Short:   "Revoke an API token by id (immediate; cannot be undone)",
		Example: `  pad token revoke <token-id>`,
		Long: `Revoke an API token by its exact id (from 'pad token list').

Revocation is immediate: anything authenticating with the token fails on
its next call. The id must be exact — there is no prefix matching, so a
typo is a not-found error rather than a wrong token revoked.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			if err := client.RevokeUserToken(args[0]); err != nil {
				return err
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Token %s revoked\n", green.Sprint("✓"), args[0])
			return nil
		},
	}
}

func tokenEditCmd() *cobra.Command {
	var nameFlag, scopesFlag string

	cmd := &cobra.Command{
		Use:   "edit <token-id> [--name <name>] [--scopes <list>]",
		Short: "Rename a token or change its scopes (the secret is unchanged)",
		Example: `  pad token edit <token-id> --name ci-deploy
  pad token edit <token-id> --scopes read
  pad token edit <token-id> --scopes '*'`,
		Long: `Change an API token's name and/or scopes by its exact id (from
'pad token list'). Only the flags you pass are changed; the secret, expiry
and everything else stay as they are.

--scopes takes the same comma-separated values as 'pad token create':
*, read, write, pad:read, pad:write, pad:admin.

Renaming, narrowing access and keeping the same access need nothing more.
Widening access (for example read to *) needs a session signed in within
the last 10 minutes; with an older one you are asked to verify your
identity first: a code from your authenticator app or a recovery code, a
code sent by email, or your password, whichever the account uses. A code
or password may be piped on stdin; an emailed code needs a terminal. An
account with none of these signs in again ('pad auth login') and widens
within 10 minutes.

Like create, this needs a login session: a call authenticated by a pad_
API token is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name, scopes *string
			if cmd.Flags().Changed("name") {
				name = &nameFlag
			}
			if cmd.Flags().Changed("scopes") {
				scopes = &scopesFlag
			}
			client, _ := getClient()
			return runTokenEdit(client, args[0], name, scopes, bufio.NewReader(os.Stdin),
				cmd.OutOrStdout(), cmd.ErrOrStderr(), cli.IsTerminal(), formatFlag == "json")
		},
	}

	cmd.Flags().StringVar(&nameFlag, "name", "", "new token name")
	cmd.Flags().StringVar(&scopesFlag, "scopes", "", "new comma-separated scopes: *, read, write, pad:read, pad:write, pad:admin")

	return cmd
}

// tokenKeyUnchanged ends every refusal token edit words itself.
const tokenKeyUnchanged = "the key is unchanged"

// runTokenEdit sends only the fields given (nil is left out, so the server
// keeps it). It PATCHes first, and runs the step-up only when the server
// answers step_up_required, then retries exactly once. Prompts go to errOut;
// the result goes to out.
func runTokenEdit(client *cli.Client, id string, name, scopes *string, in *bufio.Reader, out, errOut io.Writer, interactive, asJSON bool) error {
	if name == nil && scopes == nil {
		return fmt.Errorf("nothing to change: pass --name and/or --scopes")
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" {
			return fmt.Errorf("--name must not be blank")
		}
		name = &trimmed
	}
	if scopes != nil {
		encoded, err := encodeTokenScopes(*scopes)
		if err != nil {
			return err
		}
		if encoded == "" {
			return fmt.Errorf("--scopes needs at least one value; allowed values: %s", strings.Join(tokenScopeVocabulary, ", "))
		}
		scopes = &encoded
	}

	var updated *models.APIToken
	err := retryAfterStepUp(client, in, errOut, interactive, tokenKeyUnchanged, func() (err error) {
		updated, err = client.UpdateUserToken(id, name, scopes)
		return err
	})
	if err != nil {
		return tokenEditError(id, err)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(updated)
	}
	green := color.New(color.FgGreen)
	fmt.Fprintf(out, "%s Token updated: %s\n", green.Sprint("✓"), updated.Name)
	fmt.Fprintf(out, "  ID:      %s\n", updated.ID)
	fmt.Fprintf(out, "  Scopes:  %s\n", tokenScopeLabel(updated.Scopes))
	fmt.Fprintf(out, "  Expires: %s\n", formatTokenExpiry(updated.ExpiresAt))
	return nil
}

// retryAfterStepUp runs call and, only when the server answers
// step_up_required, runs the step-up prompt and retries exactly once.
// Prompts go to errOut. A refusal of the retry is returned as the server's
// error, worded for a person.
func retryAfterStepUp(client *cli.Client, in *bufio.Reader, errOut io.Writer, interactive bool, unchanged string, call func() error) error {
	err := call()
	if !isStepUpRequired(err) {
		return err
	}
	if err := runStepUp(client, in, errOut, interactive, unchanged); err != nil {
		return err
	}
	err = call()
	if isStepUpRequired(err) {
		return &tokenEditErr{msg: "verification did not take effect; run the command again", err: err}
	}
	return err
}

// keyStepUpError words the step-up refusals create and rotate share with
// edit; any other error is returned as it came.
func keyStepUpError(err error, unchanged string) error {
	var apiErr *cli.APIError
	switch {
	case errors.Is(err, errStepUpUnavailable), errors.As(err, &apiErr) && apiErr.Code == "step_up_unavailable":
		return &tokenEditErr{msg: signInAgainForKeys + unchanged, err: err}
	case errors.As(err, &apiErr) && apiErr.Code == "step_up_factor_mismatch":
		return &tokenEditErr{msg: "your verification method changed; run the command again", err: err}
	}
	return err
}

// signInAgainForKeys is the remedy for an account with no verification factor.
const signInAgainForKeys = "No verification method is available for this account: sign in again ('pad auth login') and run the command within 10 minutes, or turn on two-factor authentication ('pad auth 2fa setup'); "

// tokenEditErr words a refusal for a person while keeping the server's
// error reachable through errors.As (the root's rate_limited marker reads it).
type tokenEditErr struct {
	msg string
	err error
}

func (e *tokenEditErr) Error() string { return e.msg }
func (e *tokenEditErr) Unwrap() error { return e.err }

// tokenEditError maps the server's refusals to what to do about them.
func tokenEditError(id string, err error) error {
	if errors.Is(err, errStepUpUnavailable) {
		return keyStepUpError(err, tokenKeyUnchanged)
	}
	var apiErr *cli.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	wrap := func(msg string) error { return &tokenEditErr{msg: msg, err: err} }
	switch {
	case apiErr.Code == "session_required":
		return wrap("Changing a key needs a signed-in session, not an API token: run 'pad auth login' (and unset PAD_TOKEN if it is set)")
	case apiErr.Status == http.StatusNotFound:
		return wrap(fmt.Sprintf("token %s not found", id))
	case apiErr.Code == "step_up_unavailable", apiErr.Code == "step_up_factor_mismatch":
		return keyStepUpError(err, tokenKeyUnchanged)
	case apiErr.Code == "conflict":
		return wrap("the key's scopes changed meanwhile; check 'pad token list' and retry")
	case apiErr.Status == http.StatusTooManyRequests:
		if apiErr.RetryAfterSeconds > 0 {
			return wrap(fmt.Sprintf("too many attempts; try again in about %ds; %s", apiErr.RetryAfterSeconds, tokenKeyUnchanged))
		}
		return wrap("too many attempts; try again later; " + tokenKeyUnchanged)
	case apiErr.Code == "email_send_failed":
		return wrap(apiErr.Message + "; " + tokenKeyUnchanged)
	}
	return err
}

// tokenScopeLabel names a key's access level the way the server enforces
// it (tokenScopeAllows): the scopes are a union, so any write-level scope
// (or a legacy unrestricted form) gives full access, read or pad:read alone
// gives reads only, and unrecognized scopes grant nothing.
func tokenScopeLabel(scopesJSON string) string {
	if strings.TrimSpace(scopesJSON) == "" {
		return "full access"
	}
	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil || scopes == nil {
		return scopesJSON
	}
	if len(scopes) == 0 {
		return "full access"
	}
	readOnly := false
	for _, scope := range scopes {
		switch scope {
		case "*", "write", "pad:write", "pad:admin":
			return "full access"
		case "read", "pad:read":
			readOnly = true
		}
	}
	if readOnly {
		return "read only"
	}
	return "no access (unrecognized scopes " + scopesJSON + ")"
}

// tokenScopeVocabulary is the server's recognized scope set
// (internal/server/middleware_auth.go tokenScopeAllows). Anything else
// is stored verbatim and then denied on every request, so the CLI
// refuses it locally rather than minting a dead token.
var tokenScopeVocabulary = []string{"*", "read", "write", "pad:read", "pad:write", "pad:admin"}

// encodeTokenScopes turns the --scopes flag's comma-separated value into
// the JSON-array string the server's scope check parses. Empty stays
// empty (the server records full access). Values are trimmed; an empty
// element or one outside the vocabulary is refused with the allowed
// list named.
func encodeTokenScopes(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var scopes []string
	for _, part := range strings.Split(raw, ",") {
		scope := strings.TrimSpace(part)
		if scope == "" {
			return "", fmt.Errorf("--scopes has an empty entry in %q; allowed values: %s", raw, strings.Join(tokenScopeVocabulary, ", "))
		}
		if !slices.Contains(tokenScopeVocabulary, scope) {
			return "", fmt.Errorf("unknown scope %q; allowed values: %s", scope, strings.Join(tokenScopeVocabulary, ", "))
		}
		scopes = append(scopes, scope)
	}
	encoded, err := json.Marshal(scopes)
	if err != nil {
		return "", fmt.Errorf("encode scopes: %w", err)
	}
	return string(encoded), nil
}

// formatTokenExpiry renders an expiry timestamp for display; a nil
// expiry means the token never expires.
func formatTokenExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return cli.FormatInstant(*t, "2006-01-02")
}
