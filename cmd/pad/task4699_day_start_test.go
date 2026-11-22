package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TASK-4699: pad auth day-start [HH:MM] shows or sets the caller's day start.
func TestTask4699_AuthDayStart(t *testing.T) {
	for _, tc := range []struct {
		name, stored, arg, format, want string
		set, bad, reject                bool
	}{
		{name: "show", stored: `"07:30"`, want: "Day starts at: 07:30"},
		{name: "unset", stored: "null", want: "Day starts at: 09:00 (default)"},
		{name: "json", stored: "null", format: "json", want: `"day_start": null`},
		{name: "set", arg: "07:30", set: true, want: "Day start set to 07:30"},
		{name: "set-json", arg: "07:30", set: true, format: "json", want: `"day_start": "07:30"`},
		{name: "short-hour", arg: "7:30", set: true, bad: true},
		{name: "past-midnight", arg: "24:00", set: true, bad: true},
		{name: "seconds", arg: "07:30:00", set: true, bad: true},
		{name: "server", arg: "07:30", set: true, reject: true, want: "server rejected day start"},
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
					if r.Method != "PATCH" || strings.TrimSpace(string(body)) != `{"day_start":"07:30"}` {
						t.Errorf("PATCH body=%s method=%s", body, r.Method)
					}
					if tc.reject {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"code":"invalid_day_start","message":"server rejected day start"}}`)
						return
					}
					fmt.Fprint(w, `{"day_start":"07:30"}`)
					return
				}
				if r.Method != "GET" {
					t.Errorf("show used %s", r.Method)
				}
				fmt.Fprintf(w, `{"day_start":%s}`, tc.stored)
			})
			args := []string{"auth", "day-start"}
			if tc.set {
				args = append(args, tc.arg)
			}
			if tc.format != "" {
				args = append(args, "--format", tc.format)
			}
			out, _, err := tzRun(t, root, args...)
			if tc.bad {
				if err == nil || hits != 0 || !strings.Contains(err.Error(), "HH:MM") {
					t.Fatalf("invalid day start reached the server: hits=%d err=%v", hits, err)
				}
				return
			}
			if tc.reject {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("server error lost: %v", err)
				}
				return
			}
			if err != nil || hits != 1 || !strings.Contains(out, tc.want) {
				t.Fatalf("out=%q err=%v hits=%d, want %q", out, err, hits, tc.want)
			}
		})
	}
}

// A server that answers the PATCH without the value (one that predates the
// setting) is reported, not taken as saved.
func TestTask4699_AuthDayStartOldServer(t *testing.T) {
	root := tzCLI(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"timezone":null}`)
	})
	if _, _, err := tzRun(t, root, "auth", "day-start", "07:30"); err == nil || !strings.Contains(err.Error(), "did not save") {
		t.Fatalf("err=%v, want did not save", err)
	}
}
