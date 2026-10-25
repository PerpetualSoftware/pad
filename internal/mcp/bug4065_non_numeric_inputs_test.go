package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
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
		// A leading zero is refused (lead ruling 5a06782e): stdio's CLI flags
		// parse base 0, so "010" is 8 there, and reading it as 10 here would
		// make one string two numbers.
		{"string 010 refused", "010", 0, false, `must be a whole number, got the string "010"`},
		{"string 00 refused", "00", 0, false, `must be a whole number, got the string "00"`},
		{"string -010 refused", "-010", 0, false, `must be a whole number, got the string "-010"`},
		{"string 0", "0", 0, true, ""},
		{"string 100 (no leading zero)", "100", 100, true, ""},
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
		// false is refused too, not skipped as absent (r3 item 2).
		{"bool false", false, 0, false, "must be a number, got a boolean"},
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
		{"uintptr", uintptr(9), 9, true, ""},
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
	// contain when the parameter is the string "7".
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
		// The buildQuery doors (lead ruling 5a06782e, Q12): each numeric key
		// goes through numericInput, so "7" forwards 7 and a non-number is
		// refused, where the server's Atoi used to default it silently.
		{[]string{"project", "standup"}, map[string]any{}, "days", top("days"), false, "days=7"},
		{[]string{"project", "changelog"}, map[string]any{}, "days", top("days"), false, "days=7"},
		{[]string{"item", "backlinks"}, map[string]any{"ref": "TASK-1"}, "limit", top("limit"), false, "limit=7"},
		{[]string{"item", "backlinks"}, map[string]any{"ref": "TASK-1"}, "offset", top("offset"), false, "offset=7"},
		{[]string{"item", "search"}, map[string]any{"query": "x"}, "limit", top("limit"), false, "limit=7"},
		{[]string{"item", "search"}, map[string]any{"query": "x"}, "offset", top("offset"), false, "offset=7"},
		{[]string{"item", "context"}, map[string]any{"ref": "TASK-1"}, "budget", top("budget"), false, "budget=7"},
		{[]string{"project", "activity"}, map[string]any{}, "limit", top("limit"), false, "limit=7"},
		// expected_seq at both write doors.
		{[]string{"item", "update"}, map[string]any{"ref": "TASK-1", "content": "x"}, "expected_seq", top("expected_seq"), true, `"expected_seq":7`},
		{[]string{"item", "patch"}, map[string]any{"ref": "TASK-1", "edits": []any{map[string]any{"old": "a", "new": "b"}}}, "expected_seq", top("expected_seq"), false, `"expected_seq":7`},
		// Q9 and Q4: line_hint and the sort_order passthroughs.
		{[]string{"item", "patch"}, map[string]any{"ref": "TASK-1"}, "line_hint", hint, false, `"line_hint":7`},
		{[]string{"collection", "update"}, map[string]any{"slug": "tasks"}, "sort_order", top("sort_order"), false, `"sort_order":7`},
		{[]string{"role", "update"}, map[string]any{"slug": "rook"}, "sort_order", top("sort_order"), false, `"sort_order":7`},
		// pr_number (b46baef8).
		{[]string{"github", "link"}, map[string]any{"item_ref": "TASK-1", "pr_url": "https://example.com/pr/7"}, "pr_number", top("pr_number"), false, `"number":7`},
		// chatSequence's doors. Validation runs before target resolution; the
		// C<n> target form needs no lookup, so "7" can be followed to the wire
		// (r1 h2 L2).
		{[]string{"chat", "show"}, map[string]any{"target": "C12"}, "since", top("since"), false, "after_seq=7"},
		{[]string{"chat", "show"}, map[string]any{"target": "C12"}, "limit", top("limit"), false, "limit=7"},
		{[]string{"chat", "send"}, map[string]any{"target": "C12", "body": "hi"}, "reply_to", top("reply_to"), false, `"reply_to_seq":7`},
		{[]string{"chat", "delete"}, map[string]any{"target": "C12"}, "seq", top("seq"), false, "/messages/7"},
		{[]string{"chat", "read"}, map[string]any{"target": "C12"}, "to", top("to"), false, `"seq":7`},
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
		{"string 010", "010"},
		{"empty string", ""},
		{"string above int64", "99999999999999999999"},
		{"bool", true},
		{"bool false", false},
		{"object", map[string]any{"n": 1.0}},
		{"array", []any{7.0}},
	}
	for _, door := range bug4065Doors() {
		for _, val := range values {
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
			t.Fatalf("door %v %s names no forwarded substring", door.cmd, door.param)
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
				if len(reached) != 0 {
					t.Fatalf("a refused pr_number reached the server: %q", reached)
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
	for _, bad := range []string{"1.5", "abc", "", "+5", "05"} {
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
		{"-5", "limit=" + itoa(itemListResourceLimit)},
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

// Absent still means the default (r1 h2 L3, r2 item 8): only null and a
// missing key are absent, and neither is refused or forwarded.
func TestBUG4065_AbsentMeansTheDefault(t *testing.T) {
	for name, set := range map[string]func(map[string]any, any){
		"present null": func(in map[string]any, v any) { in["limit"] = v },
		"missing key":  func(map[string]any, any) {},
	} {
		door := bug4065Door{cmd: []string{"item", "list"}, input: map[string]any{}, param: "limit", set: set}
		res, reached := bug4065Dispatch(t, door, nil)
		if res != nil && res.IsError && strings.Contains(textOf(res), string(ErrValidationFailed)) {
			t.Fatalf("%s limit was refused: %s", name, textOf(res))
		}
		if len(reached) == 0 {
			t.Fatalf("%s limit reached no handler", name)
		}
		for _, r := range reached {
			if strings.Contains(r, "limit=<nil>") || strings.Contains(r, "limit=null") {
				t.Fatalf("%s limit was forwarded: %q", name, r)
			}
		}
	}
	// A null expected_seq is absent too, so the write stays UNGUARDED, as
	// before: no expected_seq goes on the wire, and it is not refused.
	door := bug4065Door{
		cmd:   []string{"item", "update"},
		input: map[string]any{"ref": "TASK-1", "content": "x"},
		param: "expected_seq",
		set:   func(in map[string]any, v any) { in["expected_seq"] = v },
	}
	res, reached := bug4065Dispatch(t, door, nil)
	if res != nil && res.IsError && strings.Contains(textOf(res), string(ErrValidationFailed)) {
		t.Fatalf("a null expected_seq was refused: %s", textOf(res))
	}
	var wrote bool
	for _, r := range reached {
		if strings.HasPrefix(r, http.MethodGet+" ") {
			continue
		}
		wrote = true
		if strings.Contains(r, "expected_seq") {
			t.Fatalf("a null expected_seq went on the wire: %q", r)
		}
	}
	if !wrote {
		t.Fatalf("a null expected_seq produced no write: %q", reached)
	}
}

// End to end through the pad_item tool handler (lead ruling 5a06782e, Q14):
// the catalog gate checks input KEYS only, so a string or a bool expected_seq
// reaches the dispatcher. Arguments are decoded from JSON bytes, as an MCP
// client's arrive.
func TestBUG4065_PadItemToolHandlerExpectedSeq(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		refuse string
		wire   any
	}{
		{"string 123 is a guarded write", `"123"`, "", 123.0},
		{"bool is refused naming expected_seq", `true`, ": expected_seq must be a number, got a boolean", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet && r.Body != nil {
					raw, _ := io.ReadAll(r.Body)
					var body map[string]any
					_ = json.Unmarshal(raw, &body)
					writes = append(writes, body)
				}
				_, _ = w.Write([]byte(`{}`))
			})
			disp := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "u"})}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
			var args map[string]any
			raw := fmt.Sprintf(`{"action":"update","workspace":"docapp","ref":"TASK-1","content":"x","expected_seq":%s}`, tc.raw)
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				t.Fatal(err)
			}
			res, err := makeFanOutHandler(padItemTool, env)(context.Background(), callToolRequest(args))
			if err != nil {
				t.Fatal(err)
			}
			if tc.refuse != "" {
				if res == nil || !res.IsError || !strings.Contains(textOf(res), tc.refuse) || !strings.Contains(textOf(res), string(ErrValidationFailed)) {
					t.Fatalf("want a validation_failed refusal containing %q, got %s", tc.refuse, textOf(res))
				}
				if len(writes) != 0 {
					t.Fatalf("a refused expected_seq still wrote: %v", writes)
				}
				return
			}
			if res != nil && res.IsError {
				t.Fatalf("refused: %s", textOf(res))
			}
			if len(writes) != 1 || writes[0]["expected_seq"] != tc.wire {
				t.Fatalf("want one write with expected_seq %v, got %v", tc.wire, writes)
			}
		})
	}
}

// The population check the lead asked for (5a06782e, Q12; rebuilt in r2 per
// b9025d7c). For every remote dispatch route and every parameter p its tool
// declares "number", the route is dispatched three ways from a curated valid
// base: p absent, p = "7", and p = each bad value. The forwarded requests are
// compared, so the oracle does not depend on the wire name p travels under:
//   - "7" forwards the same as absent: the route does not read p, and a bad
//     value must forward the same as absent too;
//   - "7" forwards differently: the route READS p, and a bad value must be
//     refused with validation_failed naming p, with no write reaching the
//     server (a route may read the item first);
//   - every variant refused for some other reason: INCONCLUSIVE, which fails,
//     so a base that does not satisfy a route cannot pass vacuously.
//
// The set of (route, p) pairs classed as reading p is pinned against
// bug4065ReadSet, so a route that stops reading p altogether cannot go green
// by being reclassified as "does not read".
//
// Conditional reads are outside the walk: a route that reads p only when an
// input the base does not set is present is classed "does not read". The
// door table covers the live case, edits[].line_hint (BUG-4065 r4, 86de95f1).
func TestBUG4065_NoRouteForwardsANonNumberForANumberParam(t *testing.T) {
	toolFor := func(cmd string) string {
		word := strings.SplitN(cmd, " ", 2)[0]
		if word == "github" {
			return "pad_item"
		}
		return "pad_" + word
	}
	params := map[string][]ParamDef{}
	for _, def := range Catalog {
		params[def.Name] = def.Schema.Params
	}
	cmds := map[string]bool{}
	for k := range routeTable {
		cmds[k] = true
	}
	for k := range (&HTTPHandlerDispatcher{}).specialRoutes() {
		cmds[k] = true
	}
	writes := func(reached []string) []string {
		var out []string
		for _, r := range reached {
			if !strings.HasPrefix(r, http.MethodGet+" ") {
				out = append(out, r)
			}
		}
		return out
	}
	run := func(cmd, p string, v any, present bool) (*CallToolResult, []string) {
		input := bug4065WalkBase(cmd)
		delete(input, p)
		if present {
			input[p] = v
		}
		door := bug4065Door{cmd: strings.Split(cmd, " "), input: input, param: p, set: func(map[string]any, any) {}}
		return bug4065Dispatch(t, door, nil)
	}
	namesP := func(res *CallToolResult, p string) bool {
		txt := textOf(res)
		return res != nil && res.IsError && strings.Contains(txt, string(ErrValidationFailed)) &&
			(strings.Contains(txt, ": "+p+" must be ") || strings.Contains(txt, ": "+p+" is "))
	}
	var reads, inconclusive []string
	for cmd := range cmds {
		for _, def := range params[toolFor(cmd)] {
			if def.Type != "number" {
				continue
			}
			p := def.Name
			resA, reqA := run(cmd, p, nil, false)
			resS, reqS := run(cmd, p, "7", true)
			if len(reqA) == 0 && len(reqS) == 0 && !namesP(resA, p) {
				inconclusive = append(inconclusive, fmt.Sprintf("%s %s: absent=%s seven=%s", cmd, p, textOf(resA), textOf(resS)))
				continue
			}
			readsP := strings.Join(reqA, "\n") != strings.Join(reqS, "\n")
			if readsP {
				reads = append(reads, cmd+" "+p)
			}
			for _, bad := range []any{true, false, "abc", map[string]any{"n": 1.0}} {
				resB, reqB := run(cmd, p, bad, true)
				if readsP {
					if !namesP(resB, p) || len(writes(reqB)) != 0 {
						t.Errorf("%s reads %s but %#v was not refused naming it (res=%s, writes=%q)", cmd, p, bad, textOf(resB), writes(reqB))
					}
					continue
				}
				if strings.Join(reqB, "\n") != strings.Join(reqA, "\n") {
					t.Errorf("%s does not read %s, yet %#v changed what it forwards: %q, absent %q", cmd, p, bad, reqB, reqA)
				}
			}
		}
	}
	for _, c := range inconclusive {
		t.Errorf("INCONCLUSIVE (the base does not get this route as far as the parameter): %s", c)
	}
	sort.Strings(reads)
	want := append([]string(nil), bug4065ReadSet...)
	sort.Strings(want)
	if strings.Join(reads, "\n") != strings.Join(want, "\n") {
		t.Errorf("the derived read-set moved; got:\n%s\nwant (bug4065ReadSet):\n%s", strings.Join(reads, "\n"), strings.Join(want, "\n"))
	}
}

// bug4065WalkBase is a curated valid input for cmd: every value is one the
// route accepts, so a refusal in the walk is about the numeric parameter.
func bug4065WalkBase(cmd string) map[string]any {
	base := map[string]any{
		"workspace": "docapp", "ref": "TASK-1", "item_ref": "TASK-1", "target": "C12",
		"query": "needle", "slug": "tasks", "attachment_id": "att-1", "seq": 3.0, "to": 3.0,
		"body": "hi", "name": "Tasks", "collection": "tasks", "title": "A title",
		"message": "a comment", "comment_id": "c-1", "reminder_id": "r-1", "status": "done",
		"decision": "ship it", "pr_number": 5.0, "pr_url": "https://example.com/pr/5",
		"content": "body text", "target_collection": "ideas", "summary": "a note",
		"artifact": "title: x\n", "code": "join-code", "email": "someone@example.com",
	}
	return base
}

// bug4065ReadSet is the checked-in set of (route, parameter) pairs the walk
// derives as reading a number-declared parameter. Review a change here
// against the route's code: a pair that disappears means a route stopped
// reading the parameter. Reviewed against the code at r2: these are the
// top-level doors of bug4065Doors (edits[].line_hint is nested, so the door
// table covers it).
var bug4065ReadSet = []string{
	"attachment list limit",
	"attachment list offset",
	"chat delete seq",
	"chat read to",
	"chat send reply_to",
	"chat show limit",
	"chat show since",
	"collection update sort_order",
	"github link pr_number",
	"item backlinks limit",
	"item backlinks offset",
	"item context budget",
	"item history limit",
	"item list limit",
	"item list offset",
	"item patch expected_seq",
	"item search limit",
	"item search offset",
	"item starred limit",
	"item update expected_seq",
	"project activity limit",
	"project changelog days",
	"project standup days",
	"role update sort_order",
	"workspace audit-log days",
	"workspace audit-log limit",
}

// The catalog doors (r3 item 4): pad_item.list and pad_item.history read limit
// through intParam themselves, before the dispatcher. A non-number is refused
// naming limit and reaches no dispatcher; "7" is the limit used.
func TestBUG4065_CatalogLimitNonNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(context.Context, map[string]any, ActionEnv) (*CallToolResult, error)
		// want is the --limit value the dispatcher receives for "7"; list asks
		// for one row past its limit (BUG-3704).
		want string
	}{
		{"pad_item.list", actionItemList, "8"},
		{"pad_item.history", actionItemHistory, "7"},
	} {
		for _, bad := range []any{"abc", true, false, map[string]any{"n": 1.0}, "010", ""} {
			t.Run(fmt.Sprintf("%s %#v", tc.name, bad), func(t *testing.T) {
				disp := &fakeDispatcher{}
				env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
				res, err := tc.act(context.Background(), map[string]any{"ref": "TASK-1", "limit": bad}, env)
				if err != nil {
					t.Fatal(err)
				}
				if res == nil || !res.IsError || !strings.Contains(textOf(res), tc.name+": limit ") {
					t.Fatalf("want a refusal naming limit, got %s", textOf(res))
				}
				if len(disp.gotPath) != 0 {
					t.Fatalf("a refused limit reached the dispatcher: %v", disp.gotPath)
				}
			})
		}
		t.Run(tc.name+" string 7", func(t *testing.T) {
			disp := &fakeDispatcher{}
			env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
			res, err := tc.act(context.Background(), map[string]any{"ref": "TASK-1", "limit": "7"}, env)
			if err != nil || res.IsError {
				t.Fatalf("\"7\" was refused: %v %s", err, textOf(res))
			}
			args := strings.Join(disp.gotArgs, " ")
			if !strings.Contains(args, "--limit "+tc.want) {
				t.Fatalf("dispatcher args %q, want --limit %s", args, tc.want)
			}
		})
	}
}
