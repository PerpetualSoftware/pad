package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestCommitmentCLIAndMCPBodyParity(t *testing.T) {
	for _, edit := range []bool{false, true} {
		for _, mode := range []string{"check", "hard", "soft", "window", "all day", "window current start"} {
			if !edit && mode == "window current start" {
				continue
			}
			name := "create/"
			if edit {
				name = "edit/"
			}
			t.Run(name+mode, func(t *testing.T) {
				input := map[string]any{"workspace": "test", "ref": "TASK-5", "every": "weekdays", "start": "2026-11-09T09:00", "audience": "agent", "anchor": "calendar", "tz": "America/New_York"}
				if mode == "hard" || mode == "soft" {
					input["deadline"] = mode
				}
				if mode == "window" || mode == "window current start" {
					input["window"] = "90m"
				}
				if mode == "window current start" {
					delete(input, "start")
				}
				if mode == "all day" {
					input["start"] = "2026-11-09"
					input["window"] = "3d"
				}
				args := []string{"item", "remind"}
				path := []string{"item", "remind"}
				if edit {
					args = append(args, "edit")
					path = append(path, "edit")
				}
				args = append(args, "TASK-5")
				for _, key := range []string{"start", "every", "deadline", "window", "audience", "anchor", "tz"} {
					if value, ok := input[key]; ok {
						args = append(args, "--"+key, value.(string))
					}
				}
				_, _, err, calls := runSched(t, okReply(weekdaysView), "", args...)
				if err != nil {
					t.Fatal(err)
				}
				var cliBody map[string]any
				_ = json.Unmarshal([]byte(calls[len(calls)-1].body), &cliBody)
				if calls[len(calls)-1].path != "/workspaces/test/items/TASK-5/commitment" {
					t.Errorf("CLI path: %s", calls[len(calls)-1].path)
				}
				var remoteBody map[string]any
				d := &mcp.HTTPHandlerDispatcher{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasSuffix(r.URL.Path, "/commitment") {
						t.Errorf("MCP path: %s", r.URL.Path)
					}
					if r.Method != "GET" {
						raw, _ := io.ReadAll(r.Body)
						if err := json.Unmarshal(raw, &remoteBody); err != nil {
							t.Fatal(err)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(weekdaysView))
				}), UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} }}
				result, err := d.Dispatch(mcp.WithDispatchInput(context.Background(), input), path, nil)
				if err != nil || result.IsError {
					t.Fatalf("MCP: %+v %v", result, err)
				}
				if !reflect.DeepEqual(cliBody, remoteBody) {
					t.Fatalf("CLI=%v MCP=%v", cliBody, remoteBody)
				}
				for _, key := range []string{"kind", "dtend"} {
					if _, ok := cliBody[key]; ok {
						t.Errorf("forbidden member %s", key)
					}
				}
				if !edit || mode != "check" {
					if _, ok := cliBody["roles"]; !ok {
						t.Error("missing roles")
					}
				}
			})
		}
	}
}

func TestCommitmentCLIErrorMarker(t *testing.T) {
	for _, code := range []string{"hard_deadline", "deadline_owner_required", "commitment_changed"} {
		t.Run(code, func(t *testing.T) {
			status := 409
			if code == "deadline_owner_required" {
				status = 403
			}
			var output strings.Builder
			writeRootStructuredError(&output, &cli.APIError{Status: status, Code: code, Message: "Refused", Details: json.RawMessage(`{"field":"deadline","code":"combination"}`)})
			if !strings.Contains(output.String(), `"code":"`+code+`"`) {
				t.Fatalf("marker: %s", output.String())
			}
		})
	}
}
