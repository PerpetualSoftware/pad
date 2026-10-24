package mcp

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4065: a string expected_seq used to read as absent, so the content write
// went out UNGUARDED. Under the lead's ruling on Q1 (af4bff40) a plain decimal
// integer string is the number it spells, so "123" must reach the server as
// expected_seq 123: a guarded write.
func TestBUG4065_StringExpectedSeqIsAGuardedWrite(t *testing.T) {
	var writes []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			writes = append(writes, body)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	input := map[string]any{"workspace": "docapp", "ref": "TASK-1", "content": "x", "expected_seq": "123"}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"item", "update"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != nil && res.IsError {
		t.Fatalf("a plain decimal string expected_seq was refused: %s", textOf(res))
	}
	if len(writes) != 1 {
		t.Fatalf("want exactly one write, got %d: %v", len(writes), writes)
	}
	got, present := writes[0]["expected_seq"]
	if !present {
		t.Fatalf("the write went out UNGUARDED: no expected_seq in %v", writes[0])
	}
	if n, ok := got.(float64); !ok || n != 123 {
		t.Fatalf("expected_seq on the wire = %#v, want 123", got)
	}
}

// BUG-4065 at the unit level: only nil is absent; every other value is a
// number numericInput can read exactly, or a refusal.
func TestBUG4065_NumericInputAdmitsOrRefusesEveryType(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		want    int64
		present bool
		wantErr string // "" = accepted
	}{
		{"nil is absent", nil, 0, false, ""},
		// Strings (lead ruling af4bff40, Q1): plain decimal integers only.
		{"string 10", "10", 10, true, ""},
		{"string -12", "-12", -12, true, ""},
		{"string -0", "-0", 0, true, ""},
		// ParseInt reads leading zeros as decimal, not octal: "010" is 10.
		{"string 010 is decimal 10", "010", 10, true, ""},
		{"string 2^53-1", "9007199254740991", 1<<53 - 1, true, ""},
		{"string 1.0", "1.0", 0, false, `must be a whole number, got the string "1.0"`},
		{"string 1e3", "1e3", 0, false, `must be a whole number, got the string "1e3"`},
		{"string leading space", " 10", 0, false, `must be a whole number, got the string " 10"`},
		{"string trailing space", "10 ", 0, false, `must be a whole number, got the string "10 "`},
		{"string +10", "+10", 0, false, `must be a whole number, got the string "+10"`},
		{"string empty", "", 0, false, `must be a whole number, got the string ""`},
		{"string lone minus", "-", 0, false, `must be a whole number, got the string "-"`},
		{"string abc", "abc", 0, false, `must be a whole number, got the string "abc"`},
		{"string 2^53", "9007199254740992", 0, false, `is beyond the exact integer range of a JSON number, got the string "9007199254740992"`},
		{"string -2^53", "-9007199254740992", 0, false, `is beyond the exact integer range of a JSON number, got the string "-9007199254740992"`},
		{"string above int64", "99999999999999999999", 0, false, `is out of range, got the string "99999999999999999999"`},
		// Other JSON types.
		{"bool", true, 0, false, "must be a number, got a boolean"},
		{"object", map[string]any{"n": 1.0}, 0, false, "must be a number, got an object"},
		{"array", []any{1.0}, 0, false, "must be a number, got an array"},
		// Native Go integers (BUG-4061's Q6 note): widened exactly.
		{"int8", int8(-5), -5, true, ""},
		{"int16", int16(300), 300, true, ""},
		{"int32", int32(-70000), -70000, true, ""},
		{"uint", uint(7), 7, true, ""},
		{"uint8", uint8(255), 255, true, ""},
		{"uint16", uint16(65535), 65535, true, ""},
		{"uint32", uint32(math.MaxUint32), math.MaxUint32, true, ""},
		{"uint64 MaxInt64", uint64(math.MaxInt64), math.MaxInt64, true, ""},
		{"uint64 above MaxInt64", uint64(math.MaxUint64), 0, false, "is out of range, got 18446744073709551615"},
		{"uint above MaxInt64", uint(1 << 63), 0, false, "is out of range, got 9223372036854775808"},
		// float32 widens exactly, then wholeInt64.
		{"float32 whole", float32(3), 3, true, ""},
		{"float32 fraction", float32(1.5), 0, false, "must be a whole number, got 1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := numericInput(tc.in)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("numericInput(%#v) = %d, %v, %v; want the refusal %q", tc.in, got, present, err, tc.wantErr)
				}
				if present {
					t.Fatalf("a refused value reported present")
				}
				return
			}
			if err != nil || got != tc.want || present != tc.present {
				t.Fatalf("numericInput(%#v) = %d, %v, %v; want %d, %v, nil", tc.in, got, present, err, tc.want, tc.present)
			}
		})
	}
}

// bug4065Door is one (door, parameter) through the remote dispatcher.
type bug4065Door struct {
	cmd   []string
	input map[string]any
	param string
	// set puts v where this door reads the parameter.
	set func(in map[string]any, v any)
	// readsFirst: the door reads the item before it validates, so a GET
	// may reach the handler; no write may.
	readsFirst bool
	// used is a substring the forwarded request (method, URI and body) must
	// contain when the parameter is the string "7". "" skips the check (chat:
	// its doors resolve a target first; chatSequence is pinned directly).
	used string
}

func bug4065Doors() []bug4065Door {
	top := func(key string) func(map[string]any, any) {
		return func(in map[string]any, v any) { in[key] = v }
	}
	hint := func(in map[string]any, v any) {
		in["edits"] = []any{map[string]any{"old": "a", "new": "b", "line_hint": v}}
	}
	return []bug4065Door{
		// The intParam doors (BUG-4061's population).
		{[]string{"attachment", "list"}, map[string]any{}, "limit", top("limit"), false, "limit=7"},
		{[]string{"attachment", "list"}, map[string]any{}, "offset", top("offset"), false, "offset=7"},
		{[]string{"item", "history"}, map[string]any{"ref": "TASK-1"}, "limit", top("limit"), false, "limit=7"},
		{[]string{"item", "starred"}, map[string]any{}, "limit", top("limit"), false, "limit=7"},
		{[]string{"workspace", "audit-log"}, map[string]any{}, "days", top("days"), false, "days=7"},
		{[]string{"workspace", "audit-log"}, map[string]any{}, "limit", top("limit"), false, "limit=7"},
		{[]string{"item", "list"}, map[string]any{}, "limit", top("limit"), false, "limit=7"},
		{[]string{"item", "list"}, map[string]any{}, "offset", top("offset"), false, "offset=7"},
		// The buildQuery doors: a string already went verbatim and still does
		// (MUST NOT CHANGE), so "7" is used; a bool or an object is refused
		// only where the key is numeric. Pinned for the refusal side below.
		{[]string{"project", "standup"}, map[string]any{}, "days", top("days"), false, "days=7"},
		{[]string{"project", "changelog"}, map[string]any{}, "days", top("days"), false, "days=7"},
		// expected_seq at both write doors.
		{[]string{"item", "update"}, map[string]any{"ref": "TASK-1", "content": "x"}, "expected_seq", top("expected_seq"), true, `"expected_seq":7`},
		{[]string{"item", "patch"}, map[string]any{"ref": "TASK-1", "edits": []any{map[string]any{"old": "a", "new": "b"}}}, "expected_seq", top("expected_seq"), false, `"expected_seq":7`},
		// Q9 and Q4: line_hint and the sort_order passthroughs.
		{[]string{"item", "patch"}, map[string]any{"ref": "TASK-1"}, "line_hint", hint, false, `"line_hint":7`},
		{[]string{"collection", "update"}, map[string]any{"slug": "tasks"}, "sort_order", top("sort_order"), false, `"sort_order":7`},
		{[]string{"role", "update"}, map[string]any{"slug": "rook"}, "sort_order", top("sort_order"), false, `"sort_order":7`},
		// pr_number (b46baef8).
		{[]string{"github", "link"}, map[string]any{"item_ref": "TASK-1", "pr_url": "https://example.com/pr/7"}, "pr_number", top("pr_number"), false, `"number":7`},
		// chatSequence's four doors. Validation runs before target resolution.
		{[]string{"chat", "show"}, map[string]any{"target": "dave"}, "since", top("since"), false, ""},
		{[]string{"chat", "show"}, map[string]any{"target": "dave"}, "limit", top("limit"), false, ""},
		{[]string{"chat", "send"}, map[string]any{"target": "dave", "body": "hi"}, "reply_to", top("reply_to"), false, ""},
		{[]string{"chat", "delete"}, map[string]any{"target": "dave"}, "seq", top("seq"), false, ""},
		{[]string{"chat", "read"}, map[string]any{"target": "dave"}, "to", top("to"), false, ""},
	}
}

func bug4065Dispatch(t *testing.T, door bug4065Door, v any) (*CallToolResult, []string) {
	t.Helper()
	var reached []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if door.readsFirst && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
		}
		reached = append(reached, r.Method+" "+r.URL.RequestURI()+" "+string(raw))
		_, _ = w.Write([]byte(`{}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	input := map[string]any{"workspace": "docapp"}
	for k, val := range door.input {
		input[k] = val
	}
	door.set(input, v)
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), door.cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res, reached
}

// BUG-4065 through the remote dispatcher: at every door, a non-number refuses
// with validation_failed naming the parameter and reaches no handler.
func TestBUG4065_EveryDoorRefusesANonNumberNamingIt(t *testing.T) {
	values := []struct {
		name string
		v    any
	}{
		{"string abc", "abc"},
		{"string 1.5", "1.5"},
		{"bool", true},
		{"object", map[string]any{"n": 1.0}},
	}
	for _, door := range bug4065Doors() {
		for _, val := range values {
			// The buildQuery doors (standup, changelog) do not know a key's
			// type: a string or a bool is forwarded verbatim for the server to
			// parse and refuse, which is the bug's stated boundary and, for a
			// string, MUST NOT CHANGE (af4bff40). They carry only the "7" row.
			if door.cmd[0] == "project" {
				continue
			}
			t.Run(strings.Join(door.cmd, " ")+" "+door.param+" "+val.name, func(t *testing.T) {
				res, reached := bug4065Dispatch(t, door, val.v)
				if res == nil || !res.IsError {
					t.Fatalf("not refused; the request reached %q", reached)
				}
				txt := textOf(res)
				if !strings.Contains(txt, ": "+door.param+" must be ") && !strings.Contains(txt, ": "+door.param+" is ") {
					t.Fatalf("refusal clause does not start with %q: %s", door.param, txt)
				}
				if !strings.Contains(txt, string(ErrValidationFailed)) {
					t.Fatalf("refusal is not a validation failure: %s", txt)
				}
				if len(reached) != 0 {
					t.Fatalf("a refused input reached the handler at %q", reached)
				}
			})
		}
	}
}

// BUG-4065 (Q1 b): the string "7" is the number 7 at every door, and that
// value is what gets forwarded, not the default.
func TestBUG4065_EveryDoorUsesAPlainDecimalString(t *testing.T) {
	for _, door := range bug4065Doors() {
		if door.used == "" {
			continue
		}
		t.Run(strings.Join(door.cmd, " ")+" "+door.param, func(t *testing.T) {
			res, reached := bug4065Dispatch(t, door, "7")
			// The stub answers {} everywhere, so a door that decodes a list
			// fails AFTER forwarding; only a refusal before it is a failure.
			if res != nil && res.IsError && strings.Contains(textOf(res), string(ErrValidationFailed)) {
				t.Fatalf("the string \"7\" was refused: %s", textOf(res))
			}
			for _, r := range reached {
				if strings.Contains(r, door.used) {
					return
				}
			}
			t.Fatalf("no forwarded request carries %q: %q", door.used, reached)
		})
	}
}

// chatSequence (BUG-4065, from 2f309abc and 30dbcd79): the 2^53 bound, an
// exact json.Number, a plain decimal string, and the message unchanged.
func TestBUG4065_ChatSequence(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
		want int64
		err  string
	}{
		{"float64 2^53-1", map[string]any{"seq": float64(1<<53 - 1)}, 1<<53 - 1, ""},
		{"float64 2^53 refused", map[string]any{"seq": float64(1 << 53)}, 0, "seq must be an integer of at least 1"},
		{"json.Number 1e6", map[string]any{"seq": json.Number("1e6")}, 1000000, ""},
		{"string 7", map[string]any{"seq": "7"}, 7, ""},
		{"string +7 refused", map[string]any{"seq": "+7"}, 0, "seq must be an integer of at least 1"},
		{"below min", map[string]any{"seq": 0.0}, 0, "seq must be an integer of at least 1"},
		{"present null", map[string]any{"seq": nil}, 0, "seq must be an integer of at least 1"},
		{"absent", map[string]any{}, 0, "seq is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chatSequence(tc.in, "seq", 1)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("chatSequence = %d, %v; want %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("chatSequence = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// pr_number (b46baef8): the bound is wholeInt64's exclusive 2^53, a json.Number
// is read exactly, and the message and hint are unchanged.
func TestBUG4065_PRNumberBoundAndJSONNumber(t *testing.T) {
	door := bug4065Door{
		cmd:   []string{"github", "link"},
		input: map[string]any{"item_ref": "TASK-1", "pr_url": "https://example.com/pr/1"},
		param: "pr_number",
		set:   func(in map[string]any, v any) { in["pr_number"] = v },
	}
	for _, tc := range []struct {
		name   string
		v      any
		refuse bool
		used   string
	}{
		{"2^53-1 accepted", float64(1<<53 - 1), false, `"number":9007199254740991`},
		{"2^53 refused", float64(1 << 53), true, ""},
		{"json.Number accepted", json.Number("42"), false, `"number":42`},
		{"zero refused", 0.0, true, ""},
		{"absent refused", nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, reached := bug4065Dispatch(t, door, tc.v)
			if tc.refuse {
				if res == nil || !res.IsError {
					t.Fatalf("not refused; reached %q", reached)
				}
				txt := textOf(res)
				if !strings.Contains(txt, "pr_number must be a positive whole number") || !strings.Contains(txt, `"hint":"Pass `+"`pr_number=\\u003cN\\u003e`"+`, the PR's number."`) {
					t.Fatalf("the message or hint changed: %s", txt)
				}
				return
			}
			if res != nil && res.IsError {
				t.Fatalf("refused: %s", textOf(res))
			}
			if len(reached) != 1 || !strings.Contains(reached[0], tc.used) {
				t.Fatalf("forwarded %q, want %q", reached, tc.used)
			}
		})
	}
}

// buildQuery (BUG-4061's Q6 note): a native unsigned value above MaxInt64 is
// refused rather than forwarded for the server to default; one within range
// is forwarded as its decimal value. Strings stay verbatim.
func TestBUG4065_BuildQueryNativeIntegers(t *testing.T) {
	keys := map[string]string{"limit": "limit"}
	if _, err := buildQuery(map[string]any{"limit": uint64(math.MaxUint64)}, keys); err == nil || err.Error() != "limit is out of range, got 18446744073709551615" {
		t.Fatalf("uint64 above MaxInt64: err = %v", err)
	}
	for _, tc := range []struct {
		v    any
		want string
	}{
		{uint64(7), "limit=7"},
		{int8(-3), "limit=-3"},
		{uint16(9), "limit=9"},
		{"anything goes", "limit=anything+goes"},
	} {
		q, err := buildQuery(map[string]any{"limit": tc.v}, keys)
		if err != nil || q != tc.want {
			t.Fatalf("buildQuery(%#v) = %q, %v; want %q", tc.v, q, err, tc.want)
		}
	}
}

// The resource limit flag (Q2): a malformed flag is refused naming limit, not
// defaulted; a plain decimal one is used; zero still means the default.
func TestBUG4065_ItemListResourceLimitFlag(t *testing.T) {
	var uris []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	f := NewHTTPResourceFetcher(&HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})})
	for _, bad := range []string{"1.5", "abc", "", "+5"} {
		uris = nil
		_, err := f.fetchItemList(context.Background(), &models.User{ID: "u"}, "docapp", map[string]string{"limit": bad})
		if err == nil || !strings.HasPrefix(err.Error(), "item list: limit ") {
			t.Fatalf("limit flag %q: err = %v, want a refusal naming limit", bad, err)
		}
		if len(uris) != 0 {
			t.Fatalf("limit flag %q reached the handler: %q", bad, uris)
		}
	}
	for _, tc := range []struct {
		flag string
		want string
	}{
		{"7", "limit=7"},
		{"0", "limit=" + itoa(itemListResourceLimit)},
	} {
		uris = nil
		if _, err := f.fetchItemList(context.Background(), &models.User{ID: "u"}, "docapp", map[string]string{"limit": tc.flag}); err != nil {
			t.Fatalf("limit flag %q: %v", tc.flag, err)
		}
		if len(uris) != 1 || !strings.Contains(uris[0], tc.want) {
			t.Fatalf("limit flag %q: requested %q, want %q", tc.flag, uris, tc.want)
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
