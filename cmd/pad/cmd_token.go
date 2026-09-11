package main

import (
	"encoding/json"
	"fmt"
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

Examples:
  pad token create --name ci-agent
  pad token create --name cursor --expires-in 30
  pad token list
  pad token revoke 7fde5e41-...`,
	}
	cmd.AddCommand(
		tokenCreateCmd(),
		tokenListCmd(),
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
		Use:   "create --name <name>",
		Short: "Mint a new API token (secret shown once)",
		Args:  cobra.NoArgs,
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

			token, err := client.CreateUserToken(input)
			if err != nil {
				// Same structured marker as webhook create (TASK-788) so
				// callers can tell a plan limit from a server fault.
				if apiErr, ok := err.(*cli.APIError); ok {
					if apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("token creation blocked: plan limit reached")
					}
				}
				return err
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

func tokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke an API token by id (immediate; cannot be undone)",
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
	return t.Format("2006-01-02")
}
