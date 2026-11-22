package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// errStepUpUnavailable is runStepUp's answer for an account with no
// verification factor. It never calls verify: the server would only refuse.
// Each caller decides what it means (2FA enrolment has a recent-sign-in
// exception; widening a key does not).
var errStepUpUnavailable = errors.New("no verification method is available for this account")

// errStepUpSignInAgain is the unavailable case where 2FA was turned on after
// this session signed in (unavailable_reason "sign_in_again"): signing in
// again makes the authenticator usable. It wraps errStepUpUnavailable, so
// callers that only ask whether a factor exists still match it.
var errStepUpSignInAgain = fmt.Errorf("this session started before two-factor authentication was turned on: %w", errStepUpUnavailable)

// runStepUp asks for whichever proof the session's current factor takes and
// verifies it once (GET /auth/step-up, then POST /auth/step-up/verify). It
// runs only after the server answered step_up_required, so it always asks.
//
// Prompts go to out. interactive is whether stdin is a terminal: an emailed
// code is refused without one BEFORE the email is sent, because the send is
// capped per hour and nobody is there to read it. A code or password piped
// on stdin is accepted. unchanged ends each refusal this function words
// itself, naming what was left as it was ("the key is unchanged").
//
// Server refusals other than a wrong code or password come back as the
// client's error, unwrapped, for the caller to map.
func runStepUp(client *cli.Client, in *bufio.Reader, out io.Writer, interactive bool, unchanged string) error {
	status, err := client.StepUpStatus()
	if err != nil {
		return err
	}
	var code, recovery, password string
	switch status.Factor {
	case "unavailable":
		if status.UnavailableReason == "sign_in_again" {
			return errStepUpSignInAgain
		}
		return errStepUpUnavailable
	case "totp":
		fmt.Fprint(out, "Code from your authenticator app (or a recovery code): ")
		entered := readStepUpLine(in)
		if entered == "" {
			return fmt.Errorf("no verification code entered; %s", unchanged)
		}
		// The same split login makes: six digits is a code from the app.
		if len(entered) == 6 && isAllDigits(entered) {
			code = entered
		} else {
			recovery = entered
		}
	case "email":
		if !interactive {
			return fmt.Errorf("verification needs a code sent by email, which needs an interactive terminal: run the command in a terminal; %s", unchanged)
		}
		if err := client.StepUpSendEmail(); err != nil {
			return err
		}
		fmt.Fprintln(out, "We emailed you a verification code.")
		fmt.Fprint(out, "Email code: ")
		code = readStepUpLine(in)
		if code == "" {
			return fmt.Errorf("no verification code entered; %s", unchanged)
		}
	case "password":
		fmt.Fprint(out, "Password: ")
		password, err = readPasswordFrom(in)
		fmt.Fprintln(out)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read password: %w", err)
		}
		if password == "" {
			return fmt.Errorf("no password entered; %s", unchanged)
		}
	default:
		return fmt.Errorf("verification factor %q is not supported by this CLI; %s", status.Factor, unchanged)
	}
	if err := client.StepUpVerify(code, recovery, password); err != nil {
		var apiErr *cli.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case "invalid_code":
				return fmt.Errorf("incorrect verification code; %s", unchanged)
			case "invalid_password":
				return fmt.Errorf("incorrect password; %s", unchanged)
			}
		}
		return err
	}
	return nil
}

// readStepUpLine reads one line; EOF with nothing typed reads as empty.
func readStepUpLine(in *bufio.Reader) string {
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// isStepUpRequired reports whether err is the server asking for a step-up.
func isStepUpRequired(err error) bool {
	var apiErr *cli.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "step_up_required"
}
