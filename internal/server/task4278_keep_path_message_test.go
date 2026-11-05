package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// TASK-4278. A keep-path error wraps entry names, paths and store
// text; none of it may reach the body. Text the handler or the store authored
// for the caller (a ValidationError's Reason) still does.
func TestKeepPathMessageNeverCarriesRawErrorText(t *testing.T) {
	const path = "/home/dave/secret-dir/bundle-7731.tar"
	const sql = "INSERT INTO items (id) VALUES ($1)"
	for name, err := range map[string]error{
		"import workspace pg": fmt.Errorf("import workspace: %w", &pgconn.PgError{Code: "23505", Message: sql + " " + path}),
		"skip entry":          fmt.Errorf("skip entry %s: unexpected EOF near %s", path, sql),
		"read blob":           fmt.Errorf("read blob %s: open %s: permission denied", path, path),
		"read manifest":       fmt.Errorf("read manifest.json: %w", errors.New(sql)),
		"manifest decode":     fmt.Errorf("manifest decode: %w (workspace created but attachments not restored)", errors.New("invalid character near "+path+" "+sql)),
		"unclassified":        fmt.Errorf("weird failure at %s (%s) SQLSTATE 57014", path, sql),
		"blob too large":      fmt.Errorf("blob %s exceeds %d-byte cap (declared %d) — raise PAD_ATTACHMENT_MAX_BYTES on this server to allow", path, 5, 9),
	} {
		t.Run(name, func(t *testing.T) {
			got := keepPathMessage(err)
			for _, leak := range []string{path, "bundle-7731", sql, "INSERT", "SQLSTATE", "57014", "23505", "EOF", "permission denied", "invalid character", "weird failure"} {
				if strings.Contains(got, leak) {
					t.Errorf("message carries %q: %s", leak, got)
				}
			}
			if got == "" {
				t.Error("empty message")
			}
		})
	}
}

func TestKeepPathMessageKeepsAuthoredText(t *testing.T) {
	err := fmt.Errorf("import workspace: %w", &store.ValidationError{Reason: `collection prefix "!!" is not importable; edit the prefix`})
	if got := keepPathMessage(err); got != `collection prefix "!!" is not importable; edit the prefix` {
		t.Errorf("a ValidationError's Reason must reach the client unchanged, got %q", got)
	}
	nul := fmt.Errorf("manifest decode: %w", &nulRefusalError{sentinel: errJSONBodyNUL, note: " (workspace created but attachments not restored)", remedy: nulRepairRemedy(nil)})
	if got := keepPathMessage(nul); !strings.Contains(got, "NUL") {
		t.Errorf("the authored NUL refusal must reach the client, got %q", got)
	}
	capped := fmt.Errorf("read tar entry: %w", &http.MaxBytesError{Limit: 7})
	if got := keepPathMessage(capped); !strings.Contains(got, "request body too large") || strings.Contains(got, "7") {
		t.Errorf("body-cap message = %q", got)
	}
	remap := &importRemapError{cause: errors.New("scan: SELECT secret")}
	if got := keepPathMessage(remap); got != remap.Error() || strings.Contains(got, "SELECT") {
		t.Errorf("remap text = %q", got)
	}
}

// An outcome-unknown write says "check before trying again", so it carries no
// Retry-After; a definitive rollback keeps it.
func TestWriteBusyRetryAfterByClass(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		write      bool
		retryAfter string
	}{
		"definitive write": {&pgconn.PgError{Code: "57014"}, true, "2"},
		"unknown write":    {context.DeadlineExceeded, true, ""},
		"read":             {context.DeadlineExceeded, false, "2"},
	} {
		rr := httptest.NewRecorder()
		writeBusy(rr, tc.err, tc.write)
		if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("%s: %d Retry-After=%q, want %q", name, rr.Code, rr.Header().Get("Retry-After"), tc.retryAfter)
		}
	}
	for unknown, want := range map[bool]string{true: "", false: "2"} {
		msg, _ := stripeCancelledBusyMessage(context.DeadlineExceeded)
		rr := httptest.NewRecorder()
		writeStripeCancelledBusy(rr, msg, unknown)
		if rr.Header().Get("Retry-After") != want {
			t.Errorf("stripe-cancelled unknown=%v: Retry-After=%q, want %q", unknown, rr.Header().Get("Retry-After"), want)
		}
	}
}

// The authored sentinels reach the client as their OWN text,
// never with the wrapper chain around them (a store prefix, a driver's Scan
// text, an SQLSTATE).
func TestKeepPathMessageSentinelsDropWrapperText(t *testing.T) {
	const scan = `Scan error on column index 3, name "fields": SQLSTATE 22P02`
	rep := &models.RepeatedMemberError{Member: "a field-values member"}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"repeated member": {fmt.Errorf("import workspace: %w: %s", rep, scan), rep.Error()},
		"nul": {fmt.Errorf("import workspace: %w: %s", &nulRefusalError{
			sentinel: errJSONBodyNUL,
			note:     " (workspace created but attachments not restored)",
			remedy:   nulRepairRemedy(&nulRepairTally{}),
		}, scan), errJSONBodyNUL.Error() + " (workspace created but attachments not restored)" + nulRepairRemedy(&nulRepairTally{})},
	} {
		t.Run(name, func(t *testing.T) {
			got := keepPathMessage(tc.err)
			if got != tc.want {
				t.Errorf("got %q, want exactly %q", got, tc.want)
			}
			if name == "nul" {
				for _, need := range []string{"--repair-nul", "pad db repair-nul", "attachments not restored"} {
					if !strings.Contains(got, need) {
						t.Errorf("message lost %q: %s", need, got)
					}
				}
			}
			for _, leak := range []string{"import workspace", "Scan error", "SQLSTATE", "22P02", `"fields"`} {
				if strings.Contains(got, leak) {
					t.Errorf("message carries %q: %s", leak, got)
				}
			}
		})
	}
}

// A remap failure whose primary wraps a ValidationError keeps the remap text.
func TestKeepPathMessageRemapBeatsValidationError(t *testing.T) {
	primary := fmt.Errorf("import workspace: %w", &store.ValidationError{Reason: "collection prefix is not importable"})
	re := &importRemapError{primary: primary, cause: errors.New("scan: SELECT secret")}
	got := keepPathMessage(re)
	if got != re.Error() || !strings.Contains(got, "attachment references") {
		t.Errorf("remap text lost: %q", got)
	}
	if strings.Contains(got, "SELECT") {
		t.Errorf("leak: %q", got)
	}
}

// No current minter puts a ValidationError and a sentinel in one
// chain (the manifest site wraps exactly one), but the order in keepPathMessage
// is deliberate and pinned: remap, then ValidationError, then the authored
// sentinels.
func TestKeepPathMessagePrecedenceValidationBeforeSentinels(t *testing.T) {
	v := &store.ValidationError{Reason: "authored reason"}
	for name, other := range map[string]error{
		"repeated": &models.RepeatedMemberError{Member: "m"},
		"nul":      &nulRefusalError{sentinel: errJSONBodyNUL, remedy: nulRepairRemedy(nil)},
	} {
		if got := keepPathMessage(errors.Join(other, v)); got != "authored reason" {
			t.Errorf("%s: got %q, want the ValidationError Reason", name, got)
		}
	}
}

// The manifest keep-path remedy is fixed text only. The
// repair's Declined reason is built from the document and can name a member
// from the payload; it must not reach the keep-path body.
func TestNulRefusalRemedyNeverCarriesDocumentText(t *testing.T) {
	tally := &nulRepairTally{Enabled: true, Declined: `repeated member "payload_member_name"`}
	if !strings.Contains(nulRepairRemedy(tally), "payload_member_name") {
		t.Fatal("precondition: the unsanitised remedy should carry the declined reason")
	}
	err := fmt.Errorf("manifest decode: %w", &nulRefusalError{
		sentinel: errJSONBodyNUL,
		note:     " (workspace created but attachments not restored)",
		remedy:   nulRepairRemedy(fixedTextRepairTally(tally)),
	})
	got := keepPathMessage(err)
	if strings.Contains(got, "payload_member_name") {
		t.Fatalf("keep-path body carries document text: %s", got)
	}
	for _, need := range []string{"pad db repair-nul", "attachments not restored"} {
		if !strings.Contains(got, need) {
			t.Errorf("body lost %q: %s", need, got)
		}
	}
	if fixedTextRepairTally(nil) != nil {
		t.Error("nil tally must stay nil")
	}
}
