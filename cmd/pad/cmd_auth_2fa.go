package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

// `pad auth 2fa setup|disable` (TASK-403). The server routes have existed
// since PR #77; these are the CLI doors onto them. The TOTP secret is shown
// once, in setup's own instructions, and nowhere else: it never reaches an
// error message, a log line or the credentials file.

// maxTOTPVerifyAttempts bounds setup's retry on a code that did not match
// (a typo, or a code that rolled over while it was typed).
const maxTOTPVerifyAttempts = 3

func twoFactorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "2fa",
		Short: "Manage two-factor authentication for your account",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(twoFactorSetupCmd(), twoFactorDisableCmd())
	return cmd
}

func twoFactorSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Turn on two-factor authentication (authenticator app)",
		Long: `Turn on two-factor authentication.

Pad shows an otpauth:// URI and its secret for your authenticator app, asks
for a code from the app, and then prints your recovery codes. The recovery
codes are shown once: store them somewhere safe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			return runTwoFactorSetup(client, bufio.NewReader(os.Stdin), cmd.OutOrStdout())
		},
	}
}

func twoFactorDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Turn off two-factor authentication",
		Long: `Turn off two-factor authentication.

An account with a password confirms with the password. An account without
one (signed up with GitHub, Google or Apple) confirms with a code from the
authenticator app or a recovery code. Turning 2FA off signs out your other
sessions; this one is kept.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			return runTwoFactorDisable(client, bufio.NewReader(os.Stdin), cmd.OutOrStdout(), func(token string) error {
				return replaceSessionToken(cfg, token)
			})
		},
	}
}

// interactiveSessionHint rewrites the server's refusal for an API token into
// what to do about it.
func interactiveSessionHint(err error) error {
	var apiErr *cli.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "forbidden" && strings.Contains(apiErr.Message, "API token") {
		return fmt.Errorf("2FA can only be changed from a signed-in session, not an API token: run 'pad auth login' (and unset PAD_TOKEN if it is set)")
	}
	return err
}

func runTwoFactorSetup(client *cli.Client, in *bufio.Reader, out io.Writer) error {
	sec, err := client.GetAccountSecurity()
	if err != nil {
		return err
	}
	if sec.TOTPEnabled {
		return fmt.Errorf("2FA is already on; to set it up again, run 'pad auth 2fa disable' first")
	}

	setup, err := client.TOTPSetup()
	if err != nil {
		return interactiveSessionHint(err)
	}

	fmt.Fprintln(out, "Add Pad to your authenticator app. Open this URI on a device that has the app,")
	fmt.Fprintln(out, "or paste it into the app:")
	fmt.Fprintf(out, "\n  %s\n\n", setup.URL)
	fmt.Fprintln(out, "Or enter the secret by hand:")
	fmt.Fprintf(out, "\n  %s\n\n", setup.Secret)

	var verified *cli.TOTPVerifyResponse
	for attempt := 1; ; attempt++ {
		fmt.Fprint(out, "Code from the app: ")
		line, _ := in.ReadString('\n')
		code := strings.TrimSpace(line)
		if code == "" {
			return fmt.Errorf("no code entered; 2FA is still off (run 'pad auth 2fa setup' again)")
		}
		verified, err = client.TOTPVerify(setup.Secret, code)
		if err == nil {
			break
		}
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "invalid_code" && attempt < maxTOTPVerifyAttempts {
			fmt.Fprintln(out, "That code didn't match. Enter the code the app shows now.")
			continue
		}
		if errors.As(err, &apiErr) && apiErr.Code == "invalid_code" {
			return fmt.Errorf("the code didn't match %d times; 2FA is still off. Check the device's clock, then run 'pad auth 2fa setup' again", maxTOTPVerifyAttempts)
		}
		return interactiveSessionHint(err)
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Fprintf(out, "\n%s 2FA is on.\n\n", green("✓"))
	fmt.Fprintln(out, "Recovery codes. Each one signs you in once if you lose the authenticator.")
	fmt.Fprintln(out, "They are shown only now: store them somewhere safe.")
	fmt.Fprintln(out)
	for _, c := range verified.RecoveryCodes {
		fmt.Fprintf(out, "  %s\n", c)
	}
	fmt.Fprintln(out)
	return nil
}

// runTwoFactorDisable asks for whichever proof the account takes: its
// password, or, for an account with none (#1879), a current code or a
// recovery code. saveToken stores the replacement session the server issues.
func runTwoFactorDisable(client *cli.Client, in *bufio.Reader, out io.Writer, saveToken func(string) error) error {
	sec, err := client.GetAccountSecurity()
	if err != nil {
		return err
	}
	if !sec.TOTPEnabled {
		return fmt.Errorf("2FA is not on for this account")
	}

	var password, code, recovery string
	if sec.PasswordSet == nil || *sec.PasswordSet {
		fmt.Fprint(out, "Password: ")
		password, err = readPasswordFrom(in)
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}
		fmt.Fprintln(out)
		if password == "" {
			return fmt.Errorf("no password entered; 2FA is still on")
		}
	} else {
		fmt.Fprintln(out, "This account has no password. Confirm with a code from your authenticator app,")
		fmt.Fprintln(out, "or one of your recovery codes.")
		fmt.Fprint(out, "Code or recovery code: ")
		line, _ := in.ReadString('\n')
		entered := strings.TrimSpace(line)
		if entered == "" {
			return fmt.Errorf("no code entered; 2FA is still on")
		}
		// The same split login makes: six digits is a code from the app.
		if len(entered) == 6 && isAllDigits(entered) {
			code = entered
		} else {
			recovery = entered
		}
	}

	resp, err := client.TOTPDisable(password, code, recovery)
	if err != nil {
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case "invalid_password":
				return fmt.Errorf("incorrect password; 2FA is still on")
			case "invalid_code":
				return fmt.Errorf("that code didn't work; 2FA is still on")
			}
		}
		return interactiveSessionHint(err)
	}

	green := color.New(color.FgGreen).SprintFunc()
	fmt.Fprintf(out, "%s 2FA is off. Your other sessions were signed out.\n", green("✓"))
	if resp.Token == "" {
		fmt.Fprintln(out, "Run 'pad auth login' to sign this CLI in again.")
		return nil
	}
	if err := saveToken(resp.Token); err != nil {
		fmt.Fprintf(out, "This CLI's session was replaced, and saving the new one failed (%v).\nRun 'pad auth login' to sign in again.\n", err)
	}
	return nil
}

// replaceSessionToken swaps the saved session for the one the server issued
// when it rotated sessions. A PAD_TOKEN session lives in the environment,
// which the CLI cannot rewrite, so that case is reported instead.
func replaceSessionToken(cfg *config.Config, token string) error {
	if os.Getenv("PAD_TOKEN") != "" {
		return fmt.Errorf("this session came from PAD_TOKEN, which the CLI cannot update")
	}
	store, err := cli.LoadStore()
	if err != nil {
		return err
	}
	creds := store.Get(cfg.BaseURL())
	if creds == nil {
		return fmt.Errorf("no saved credentials for %s", cfg.BaseURL())
	}
	creds.Token = token
	store.Set(cfg.BaseURL(), creds)
	return store.Save()
}
