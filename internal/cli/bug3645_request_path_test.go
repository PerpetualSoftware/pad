package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// BUG-3645: a workspace string from .pad.toml or --workspace was concatenated
// raw into request paths, so "victim/#" turned an item delete into
// DELETE /workspaces/victim/ (the rest was a fragment, never sent).

func TestBUG3645_CheckRequestPath(t *testing.T) {
	ok := []string{
		"",
		"/health",
		"/workspaces/w/items/TASK-1",
		"/workspaces/w%2F%23/items/TASK-1",
		"/workspaces/w/items?limit=5&q=a%2Fb",
		"/workspaces/w/items/x..y",
		"/workspaces/w/items/.hidden",
	}
	for _, p := range ok {
		if err := checkRequestPath(p); err != nil {
			t.Errorf("checkRequestPath(%q) = %v, want nil", p, err)
		}
	}
	refused := []string{
		"/workspaces/victim/#/items/TASK-1",
		"/workspaces/victim?/items/TASK-1",
		"/workspaces/../items/TASK-1",
		"/workspaces/./items",
		"/workspaces//items",
		"/workspaces/w/items/..",
		"/",
	}
	for _, p := range refused {
		if err := checkRequestPath(p); err == nil {
			t.Errorf("checkRequestPath(%q) = nil, want a refusal", p)
		}
	}
}

// recordingServer answers every request 404 and records its escaped path and
// raw query.
func recordingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"nope"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func isolatedClient(t *testing.T, base string) *Client {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PAD_TOKEN", "")
	c := NewClientFromURL(base)
	c.httpClient.Timeout = 2 * time.Second
	return c
}

// The reported scenario, end to end: the DELETE must not reach the
// workspace, escaped or refused.
func TestBUG3645_ItemDeleteWithHostileWorkspaceNeverHitsTheWorkspace(t *testing.T) {
	srv, seen := recordingServer(t)
	c := isolatedClient(t, srv.URL)
	for _, ws := range []string{"victim/#", "victim?", "victim/..", ".."} {
		_ = c.DeleteItem(ws, "TASK-1")
		_, _ = c.GetItem(ws, "TASK-1")
	}
	for _, req := range seen() {
		path, _, _ := strings.Cut(strings.SplitN(req, " ", 2)[1], "?")
		if path == "/api/v1/workspaces/victim" || path == "/api/v1/workspaces/victim/" || path == "/api/v1/workspaces" || path == "/api/v1/items/TASK-1" {
			t.Errorf("request re-routed by the workspace string: %s", req)
		}
	}
}

// The census instrument. Every exported *Client method is called with the
// same hostile string in every string argument (zero values elsewhere). A
// request that reaches the server must carry that string escaped inside one
// segment, or carry none of its pieces: a decoded path or query holding "/HX/"
// or "HY" without the whole hostile value is a re-route.
//
// It is a population instrument, not a sample: a builder added later without
// PathEscape fails here without anyone listing it.
func TestBUG3645_EveryClientMethodKeepsHostileStringsInOneSegment(t *testing.T) {
	const hostile = "HX/HY?HZ#HW"
	escaped := "HX%2FHY%3FHZ%23HW"
	srv, seen := recordingServer(t)
	c := isolatedClient(t, srv.URL)

	// Methods whose string argument IS the request path, by contract. The
	// path builders' callers own escaping for these; only the newRequest
	// chokepoint stands between them and a re-route, and the census checks
	// that it refuses them (no request reaches the server).
	rawPath := map[string]bool{
		"RawGet": true, "RawStream": true, "PostRaw": true, "PostRawWithContentType": true,
		"PostStreamContext": true, "PostStreamWithContentType": true,
		"PostStreamWithContentTypeHeaders": true,
	}

	cv := reflect.ValueOf(c)
	ct := cv.Type()
	var called, skipped []string
	byMethod := map[string][]string{}
	for i := 0; i < ct.NumMethod(); i++ {
		m := ct.Method(i)
		mt := m.Type
		hasString := false
		args := []reflect.Value{cv}
		for j := 1; j < mt.NumIn(); j++ {
			in := mt.In(j)
			if in.Kind() == reflect.String {
				hasString = true
				args = append(args, reflect.ValueOf(hostile).Convert(in))
				continue
			}
			args = append(args, argFor(in))
		}
		if !hasString || mt.IsVariadic() {
			continue
		}
		before := len(seen())
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = recover() }()
			m.Func.Call(args)
		}()
		select {
		case <-done:
			called = append(called, m.Name)
			byMethod[m.Name] = seen()[before:]
		case <-time.After(5 * time.Second):
			skipped = append(skipped, m.Name+" (timeout)")
		}
	}
	sort.Strings(skipped)
	t.Logf("called %d methods with a string argument; skipped %v", len(called), skipped)
	if len(called) < 60 {
		t.Fatalf("only %d methods were exercised; the instrument has lost its population", len(called))
	}

	for name := range rawPath {
		if _, ok := byMethod[name]; !ok {
			t.Errorf("raw-path method %s was not exercised; drop it from the allowlist or find why", name)
		}
	}
	reached := 0
	for name, reqs := range byMethod {
		if rawPath[name] {
			if len(reqs) != 0 {
				t.Errorf("%s sent a hostile raw path the chokepoint should refuse: %v", name, reqs)
			}
			continue
		}
		for _, req := range reqs {
			checkCensusRequest(t, name, req, escaped, &reached)
		}
	}
	t.Logf("%d requests carried the hostile string escaped in their path", reached)
	// Measured 86 when written (BUG-3645). A builder that stops escaping
	// drops below this: its hostile path is refused by the chokepoint
	// instead of sent escaped.
	if reached < 86 {
		t.Fatalf("only %d requests carried the hostile string in their path; expected the census population", reached)
	}
}

func checkCensusRequest(t *testing.T, method, req, escaped string, reached *int) {
	t.Helper()
	{
		target := strings.SplitN(req, " ", 2)[1]
		if strings.Contains(target, "#") {
			t.Errorf("%s: raw '#' reached the server: %s", method, req)
		}
		path, _, _ := strings.Cut(target, "?")
		if strings.Contains(path, "HX") {
			*reached++
			if !strings.Contains(path, escaped) {
				t.Errorf("%s: hostile string split across segments: %s", method, req)
			}
		}
		if strings.Contains(path, "/HY") || strings.HasSuffix(path, "/HX") {
			t.Errorf("%s: hostile string re-routed the path: %s", method, req)
		}
	}
}

// argFor returns a usable non-string argument: a context, a buffer for any
// interface it satisfies (io.Reader, io.Writer), a fresh value behind a
// pointer, an empty map; the zero value otherwise. Nil readers and writers
// panic inside the methods' own goroutines, where no recover reaches.
func argFor(in reflect.Type) reflect.Value {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	switch {
	case in == ctxType:
		return reflect.ValueOf(context.Background())
	case in.Kind() == reflect.Interface && reflect.TypeOf(&bytes.Buffer{}).Implements(in):
		return reflect.ValueOf(&bytes.Buffer{}).Convert(in)
	case in.Kind() == reflect.Pointer:
		return reflect.New(in.Elem())
	case in.Kind() == reflect.Map:
		return reflect.MakeMap(in)
	}
	return reflect.Zero(in)
}
