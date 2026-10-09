package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// TestScopePopulation_WriteActionsAreRefusedForReadTokens is the
// population table behind the /mcp insufficient_scope pre-check
// (TASK-2308). The pre-check answers 403 for a read-only token calling
// any action CallNeedsWriteScope calls a write, before dispatch runs. That
// is only safe if dispatch would have refused the same call: every
// non-read-only action must issue a mutating request, which the
// dispatcher's own scope check (buildAuthedRequest) refuses for a
// read token.
//
// Each action is driven through its real ActionFn and the real
// HTTPHandlerDispatcher, once with a pad:write token (the control: every
// write action must succeed AND send a mutating request) and once with a
// pad:read token (every write action must be refused by the scope check,
// with no mutating request reaching the handler). The handler answers the
// prefetches the actions make with values they accept.
func TestScopePopulation_WriteActionsAreRefusedForReadTokens(t *testing.T) {
	doc := liveCmdhelpDoc(t)

	// CONTROL: under a pad:write token every non-read-only action runs to
	// a SUCCESS result against this handler. Without it, a read-token
	// error below could be a fixture failure (the handler's answer did not
	// satisfy a prefetch) rather than the scope refusal, and the test
	// would pass with nothing measured.
	write := runScopePopulation(t, doc, `["pad:write"]`)
	read := runScopePopulation(t, doc, `["pad:read"]`)

	var controlFailed, notRefused, readRefused []string
	for _, label := range sortedLabels(write) {
		w, r := write[label], read[label]
		if w.readOnly {
			if r.denied {
				readRefused = append(readRefused, label)
			}
			continue
		}
		if bug := knownBrokenOverHTTP[label]; bug != "" {
			// No control can succeed, but the read token must still get a
			// refusal, or the 403 would replace something that worked.
			if !r.isError || r.mutated {
				notRefused = append(notRefused, label+" (exempt as "+bug+", but not refused for a read token)")
			}
			continue
		}
		if w.isError || !w.mutated {
			controlFailed = append(controlFailed, label+": "+w.text)
			continue
		}
		// Refused means: dispatch's scope check fired and nothing mutating
		// reached the handler. bulk-update answers a success envelope
		// whose every row failed, so the result's IsError is not the test.
		if !r.denied || r.mutated {
			notRefused = append(notRefused, label)
		}
	}
	t.Logf("read-only actions a read token is still refused (tool error, not 403):\n  %s", strings.Join(readRefused, "\n  "))
	if len(controlFailed) > 0 {
		t.Errorf("control: these write actions did not succeed under a pad:write token, so their read-token result measures nothing:\n  %s",
			strings.Join(controlFailed, "\n  "))
	}
	if len(notRefused) > 0 {
		t.Errorf("non-read-only actions a read token is NOT refused by dispatch; the /mcp pre-check would 403 a call that works today:\n  %s",
			strings.Join(notRefused, "\n  "))
	}
}

// knownBrokenOverHTTP lists write actions that fail over the HTTP
// transport for every token, so no control run can succeed. Their
// read-token run is still checked: it must be an error with nothing
// mutating reaching the handler, so the 403 replaces a refusal, never a
// success. Remove the entry when its bug is fixed.
// Empty since BUG-3533 fixed pad_item.import, its one entry.
var knownBrokenOverHTTP = map[string]string{}

type scopePopulationResult struct {
	readOnly bool
	isError  bool
	denied   bool // the dispatcher's scope check refused a request
	mutated  bool // a non-GET request reached the handler
	text     string
}

func sortedLabels(m map[string]scopePopulationResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scopePopulationHandler answers the prefetches the write actions make
// (an item by ref, an item's links) with values they accept, and every
// other request with an empty object.
func scopePopulationHandler(onMutate func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			onMutate()
		}
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/docapp/")
		parts := strings.Split(p, "/")
		if r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "items" && parts[2] == "links" {
			var links []map[string]string
			for linkType := range itemLinkRoutes {
				lt, _ := models.NormalizeItemLinkType(linkType)
				links = append(links, map[string]string{"id": "l-" + lt, "source_id": "i-1", "target_id": "i-1", "link_type": lt})
			}
			_ = json.NewEncoder(w).Encode(links)
			return
		}
		if r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "items" {
			_, _ = w.Write([]byte(`{"id":"i-1","slug":"task-1","ref":"TASK-1","collection_slug":"tasks","fields":"{}"}`))
			return
		}
		_, _ = w.Write([]byte("{}"))
	})
}

func runScopePopulation(t *testing.T, doc *cmdhelp.Document, scopes string) map[string]scopePopulationResult {
	t.Helper()
	results := map[string]scopePopulationResult{}
	drive := func(label string, readOnly bool, handler ActionFn, input map[string]any) {
		var mu sync.Mutex
		res := scopePopulationResult{readOnly: readOnly}
		disp := &HTTPHandlerDispatcher{
			Handler: scopePopulationHandler(func() {
				mu.Lock()
				res.mutated = true
				mu.Unlock()
			}),
			UserResolver: func(context.Context) *models.User {
				return &models.User{ID: "u-1", Email: "u@example.com", Name: "U"}
			},
			OnScopeDenied: func(string, string) {
				mu.Lock()
				res.denied = true
				mu.Unlock()
			},
		}
		env := ActionEnv{Doc: doc, Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
		ctx := server.WithTokenScopes(context.Background(), scopes)
		out, err := handler(ctx, input, env)
		switch {
		case err != nil:
			res.isError, res.text = true, err.Error()
		case out == nil:
			res.isError, res.text = true, "nil result"
		case out.IsError:
			res.isError, res.text = true, resultText(out)
		}
		results[label] = res
	}
	for _, def := range Catalog {
		for actionName, handler := range def.Actions {
			key := def.Name + "." + actionName
			ro := !CallNeedsWriteScope(def.Name, actionName)
			if def.Name == "pad_item" && (actionName == "link" || actionName == "unlink") {
				for linkType := range itemLinkRoutes {
					input := parityFixtureInput()
					input["workspace"] = "docapp"
					input["link_type"] = linkType
					input["target"] = "TASK-2"
					drive(key+"("+linkType+")", ro, handler, input)
				}
				continue
			}
			input := parityFixtureInput()
			input["workspace"] = "docapp"
			if key == "pad_library.activate" {
				input["title"] = "Ship tasks" // a real library entry
			}
			drive(key, ro, handler, input)
		}
	}
	return results
}

func resultText(out *CallToolResult) string {
	if out == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range out.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
