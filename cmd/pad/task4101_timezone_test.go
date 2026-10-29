package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

const tzNotice = "times shown in UTC; set yours with pad auth timezone <IANA>\n"

func tzCLI(t *testing.T, handler http.HandlerFunc) *cobra.Command {
	t.Helper()
	t.Setenv("TZ", "UTC")
	old := time.Local
	time.Local = time.FixedZone("Machine", 9*3600)
	t.Cleanup(func() { time.Local = old })
	setTempHomeMain(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_test")
	root := newRootCmd()
	t.Cleanup(func() { formatFlag = "table"; workspaceFlag = ""; cli.SetDisplayLocationResolver(nil) })
	return root
}

func tzRun(t *testing.T, root *cobra.Command, args ...string) (out, stderr string, err error) {
	t.Helper()
	root.SetArgs(append([]string{"--workspace", "test"}, args...))
	stderr = captureStderr(t, func() { out = captureStdout(t, func() { err = root.Execute() }) })
	return
}

func tzStub(zone string, hits *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/me"):
			*hits++
			fmt.Fprintf(w, `{"name":"Viewer","timezone":%s}`, zone)
		case strings.HasSuffix(r.URL.Path, "/claim"):
			fmt.Fprint(w, `{"lease":{"holder":"viewer","expires_at":"2026-01-15T18:30:00Z"}}`)
		case strings.HasSuffix(r.URL.Path, "/dashboard"):
			note := ""
			z := zone
			if z == "null" {
				z = `"UTC"`
				note = "unset"
			}
			fmt.Fprintf(w, `{"timezone":%s,"timezone_note":%q,"suggested_next":[]}`, z, note)
		case strings.HasSuffix(r.URL.Path, "/auth/tokens"):
			fmt.Fprint(w, `[{"id":"a","created_at":"2026-01-15T18:30:00Z","expires_at":"2026-01-15T18:30:00Z"},{"id":"b","created_at":"2026-01-15T18:30:00Z","expires_at":"2026-01-15T18:30:00Z"}]`)
		default:
			fmt.Fprint(w, `[]`)
		}
	}
}

func TestTZ4101_LeaseRendersInUserZone(t *testing.T) {
	hits := 0
	root := tzCLI(t, tzStub(`"America/New_York"`, &hits))
	out, stderr, err := tzRun(t, root, "item", "claim", "TASK-1")
	if err != nil || !strings.Contains(out, "13:30:00") || stderr != "" || hits != 1 {
		t.Fatalf("user zone missing: out=%q stderr=%q hits=%d err=%v", out, stderr, hits, err)
	}
}

func TestTZ4101_LeaseUnsetIsUTCWithNotice(t *testing.T) {
	hits := 0
	root := tzCLI(t, tzStub("null", &hits))
	out, stderr, err := tzRun(t, root, "item", "claim", "TASK-1")
	if err != nil || !strings.Contains(out, "18:30:00") || stderr != tzNotice || strings.Contains(out, "times shown") {
		t.Fatalf("UTC/notice missing: out=%q stderr=%q err=%v", out, stderr, err)
	}
}

func TestTZ4101_JSONHasNoNotice(t *testing.T) {
	t.Run("standup-user-day", func(t *testing.T) {
		hits := 0
		// A viewer day that differs from UTC's at any hour: Kiritimati is a day
		// ahead from 10:00 UTC, Pago_Pago a day behind before 11:00 UTC.
		zone := "Pacific/Kiritimati"
		if time.Now().UTC().Hour() < 10 {
			zone = "Pacific/Pago_Pago"
		}
		loc, _ := time.LoadLocation(zone)
		root := tzCLI(t, tzStub(fmt.Sprintf("%q", zone), &hits))
		// The machine day equals the UTC day, so one date excludes both.
		time.Local = time.UTC
		utcBefore := time.Now().UTC().Format("2006-01-02")
		before := time.Now().In(loc).Format("2006-01-02")
		out, stderr, err := tzRun(t, root, "project", "standup", "--format", "json")
		after := time.Now().In(loc).Format("2006-01-02")
		utcAfter := time.Now().UTC().Format("2006-01-02")
		var result struct {
			Date string `json:"date"`
		}
		if json.Unmarshal([]byte(out), &result) != nil || err != nil || stderr != "" || hits != 0 || (result.Date != before && result.Date != after) || result.Date == utcBefore || result.Date == utcAfter {
			t.Fatalf("standup JSON day: out=%q stderr=%q hits=%d err=%v utc=%s", out, stderr, hits, err, utcBefore)
		}
	})
	for _, args := range [][]string{{"item", "claim", "TASK-1"}, {"project", "next"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			hits := 0
			root := tzCLI(t, tzStub("null", &hits))
			out, stderr, err := tzRun(t, root, append(args, "--format", "json")...)
			if err != nil || stderr != "" || hits != 0 || !json.Valid([]byte(out)) {
				t.Fatalf("JSON changed: out=%q stderr=%q hits=%d err=%v", out, stderr, hits, err)
			}
			if args[0] == "item" && !strings.Contains(out, "2026-01-15T18:30:00Z") {
				t.Fatal("raw expiry changed")
			}
		})
	}
}

func TestTZ4101_NextNoticeFromDashboard(t *testing.T) {
	for _, command := range []string{"next", "dashboard", "ready", "stale", "standup"} {
		for _, zone := range []string{"null", `"Pacific/Kiritimati"`} {
			t.Run(command+zone, func(t *testing.T) {
				hits := 0
				root := tzCLI(t, tzStub(zone, &hits))
				_, stderr, err := tzRun(t, root, "project", command)
				want := ""
				if zone == "null" {
					want = tzNotice
				}
				if err != nil || stderr != want || hits != 0 {
					t.Fatalf("dashboard seeding/notice: stderr=%q want=%q hits=%d err=%v", stderr, want, hits, err)
				}
			})
		}
	}
}

func TestTZ4101_NoticeOncePerProcess(t *testing.T) {
	hits := 0
	root := tzCLI(t, tzStub("null", &hits))
	_, stderr, err := tzRun(t, root, "token", "list")
	if err != nil || stderr != tzNotice || hits != 1 {
		t.Fatalf("once notice: stderr=%q hits=%d err=%v", stderr, hits, err)
	}
}

func TestTZ4101_AuthTimezone(t *testing.T) {
	for _, tc := range []struct {
		name, zone, arg, format, want string
		set, bad, reject              bool
	}{
		{name: "show", zone: `"America/New_York"`, want: "Time zone: America/New_York"},
		{name: "unset", zone: "null", want: "pad auth timezone <IANA>"},
		{name: "json", zone: "null", format: "json", want: `"timezone": null`},
		{name: "set", arg: "Europe/Paris", set: true, want: "Time zone set to Europe/Paris"},
		{name: "set-json", arg: "Europe/Paris", set: true, format: "json", want: `"timezone": "Europe/Paris"`},
		{name: "invalid", arg: "Mars/Base", set: true, bad: true}, {name: "local", arg: "Local", set: true, bad: true}, {name: "empty", set: true, bad: true},
		{name: "server", arg: "Europe/Paris", set: true, reject: true, want: "server rejected zone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			root := tzCLI(t, func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.URL.Path != "/api/v1/auth/me" {
					t.Errorf("path=%s", r.URL.Path)
				}
				if tc.set {
					body, _ := io.ReadAll(r.Body)
					if r.Method != "PATCH" || strings.TrimSpace(string(body)) != `{"timezone":"Europe/Paris"}` {
						t.Errorf("PATCH body=%s method=%s", body, r.Method)
					}
					if tc.reject {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"code":"invalid_tz","message":"server rejected zone"}}`)
						return
					}
					fmt.Fprint(w, `{"timezone":"Europe/Paris"}`)
				} else {
					fmt.Fprintf(w, `{"timezone":%s}`, tc.zone)
				}
			})
			args := []string{"auth", "timezone"}
			if tc.set {
				args = append(args, tc.arg)
			}
			if tc.format != "" {
				args = append(args, "--format", tc.format)
			}
			out, _, err := tzRun(t, root, args...)
			if tc.bad {
				if err == nil || hits != 0 || !strings.Contains(err.Error(), "IANA") {
					t.Fatalf("invalid zone reached server: hits=%d err=%v", hits, err)
				}
				return
			}
			if tc.name == "unset" && strings.Contains(out, "times shown") {
				t.Fatal("stderr-only notice leaked to stdout")
			}
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("server error lost: %v", err)
				}
				return
			}
			if err != nil || hits != 1 || !strings.Contains(out, tc.want) {
				t.Fatalf("timezone command: out=%q hits=%d err=%v", out, hits, err)
			}
		})
	}
}

func TestTZ4101_WhoamiCarriesTimezone(t *testing.T) {
	hits := 0
	root := tzCLI(t, tzStub(`"America/New_York"`, &hits))
	out, _, err := tzRun(t, root, "auth", "whoami", "--format", "json")
	if err != nil || !strings.Contains(out, `"timezone": "America/New_York"`) {
		t.Fatalf("whoami zone missing: %s %v", out, err)
	}
	formatFlag = "table"
	out = captureStdout(t, func() { cmd := whoamiCmd(); err = cmd.RunE(cmd, nil) })
	if err != nil || !strings.Contains(out, "Zone:  America/New_York") {
		t.Fatalf("human zone missing: %s %v", out, err)
	}
}

func TestTZ4101_InstantPopulation(t *testing.T) {
	instant := "2026-01-15T02:30:00Z"
	row := `[{"created_at":"` + instant + `","expires_at":"` + instant + `","deleted_at":"` + instant + `"}]`
	for _, tc := range []struct{ args, body, want string }{
		{"attachment list", `{"attachments":` + row + `}`, "2026-01-14 21:30"},
		{"token list", row, "2026-01-14"},
		{"project activity", row, "2026-01-14 21:30"},
		{"project activity --format markdown", row, "2026-01-14 21:30"},
		{"project watch", fmt.Sprintf("event: item_created\ndata: {\"timestamp\":%d}\n\n", time.Date(2026, 1, 15, 2, 30, 0, 0, time.UTC).UnixMilli()), "21:30:00"},
		{"item history TASK-1", row, "2026-01-14 21:30:00"},
		{"workspace deleted", row, "Jan 14, 2026"},
		{"workspace audit-log", row, "2026-01-14 21:30"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			hits := 0
			root := tzCLI(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/auth/me") {
					hits++
					fmt.Fprint(w, `{"timezone":"America/New_York"}`)
					return
				}
				fmt.Fprint(w, tc.body)
			})
			out, stderr, err := tzRun(t, root, strings.Fields(tc.args)...)
			if err != nil || stderr != "" || hits != 1 || !strings.Contains(out, tc.want) {
				t.Fatalf("instant population: out=%q stderr=%q hits=%d err=%v", out, stderr, hits, err)
			}
		})
	}
}

func TestTZ4101_AuthTimezoneRefusesUnsavedZone(t *testing.T) {
	for name, answer := range map[string]string{
		"omitted":   `{"name":"Viewer"}`,
		"different": `{"timezone":"Asia/Tokyo"}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := tzCLI(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, answer) })
			out, _, err := tzRun(t, root, "auth", "timezone", "Europe/Paris")
			if err == nil || !strings.Contains(err.Error(), "did not save the time zone") || strings.Contains(out, "set to") {
				t.Fatalf("unsaved zone reported as set: out=%q err=%v", out, err)
			}
		})
	}
}

// The resolver must never exit or prompt: newRootCmd installs it in every
// test binary and every command, configured or not.
func TestTZ4101_ResolverUnconfiguredNeverExits(t *testing.T) {
	if os.Getenv("TZ4101_SUBPROCESS") == "1" {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
		defer srv.Close()
		setTempHomeMain(t)
		t.Setenv("PAD_URL", "")
		t.Setenv("PAD_TOKEN", "")
		time.Local = time.FixedZone("Machine", 9*3600)
		newRootCmd()
		old := time.Date(2026, 1, 15, 2, 30, 0, 0, time.UTC)
		fmt.Printf("INSTANT=%s HITS=%d\n", cli.FormatInstant(old, "2006-01-02 15:04"), hits)
		fmt.Printf("RELATIVE=%s\n", cli.RelativeTime(time.Now().Add(-30*24*time.Hour)))
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTZ4101_ResolverUnconfiguredNeverExits$")
	cmd.Env = append(os.Environ(), "TZ4101_SUBPROCESS=1")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil || !strings.Contains(stdout.String(), "INSTANT=2026-01-15 02:30 HITS=0") || strings.Contains(stderr.String(), "times shown") || strings.Contains(stderr.String(), "not configured") {
		t.Fatalf("resolver exited, prompted or left UTC: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}
