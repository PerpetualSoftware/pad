package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
)

const testSecret = "s3cret-value"

func twoMsgs() []Message {
	a, _ := NewMessage("m0", "android", "tok-AAA", KindAssigned, "n1", "c1", 1)
	b, _ := NewMessage("m1", "ios", "tok-BBB", KindSummary, "n2", "c2", 4)
	return []Message{a, b}
}

func serve(t *testing.T, h http.HandlerFunc) *Gateway {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewGateway(srv.URL, testSecret)
}

func okResults(w http.ResponseWriter, results string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"results":%s}`, results)
}

func TestGateway_RequestShapeAndHeader(t *testing.T) {
	var got struct {
		Messages []Message `json:"messages"`
	}
	var secret, ctype, method string
	g := serve(t, func(w http.ResponseWriter, r *http.Request) {
		secret, ctype, method = r.Header.Get("X-Cloud-Secret"), r.Header.Get("Content-Type"), r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		okResults(w, `[{"id":"m0","status":"sent"},{"id":"m1","status":"invalid_token"}]`)
	})
	res, err := g.Send(context.Background(), twoMsgs())
	if err != nil {
		t.Fatal(err)
	}
	if secret != testSecret || ctype != "application/json" || method != "POST" {
		t.Fatalf("secret=%q ctype=%q method=%q", secret, ctype, method)
	}
	if len(got.Messages) != 2 || got.Messages[1].Data != (Data{NID: "n2", CID: "c2", Kind: "summary", Count: 4}) {
		t.Fatalf("request = %+v", got)
	}
	if res[0].Status != StatusSent || res[1].Status != StatusInvalidToken {
		t.Fatalf("results = %+v", res)
	}
}

func TestGateway_WholeRequestStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{400, ErrBadRequest}, {403, ErrUnavailable}, {500, ErrUnavailable}, {503, ErrUnavailable}, {302, ErrUnavailable}, {204, ErrUnavailable},
	} {
		g := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.status == 302 {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(tc.status)
		})
		res, err := g.Send(context.Background(), twoMsgs())
		if !errors.Is(err, tc.want) || res != nil {
			t.Errorf("status %d: res=%v err=%v, want %v", tc.status, res, err, tc.want)
		}
		if tc.want == ErrUnavailable && errors.Is(err, ErrBadRequest) {
			t.Errorf("status %d mapped to both", tc.status)
		}
	}
}

func TestGateway_RedirectNotFollowed_SecretNotForwarded(t *testing.T) {
	var hits int32
	var leaked atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		leaked.Store(r.Header.Get("X-Cloud-Secret"))
		okResults(w, `[]`)
	}))
	defer target.Close()
	g := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := g.Send(context.Background(), twoMsgs())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("redirect target was called with %v", leaked.Load())
	}
}

func TestGateway_ResultValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"short":        {`{"results":[{"id":"m0","status":"sent"}]}`, []string{"retry", "retry"}},
		"long":         {`{"results":[{"id":"m0","status":"sent"},{"id":"m1","status":"sent"},{"id":"m2","status":"sent"}]}`, []string{"retry", "retry"}},
		"wrong id":     {`{"results":[{"id":"m0","status":"sent"},{"id":"zz","status":"sent"}]}`, []string{"retry", "retry"}},
		"reordered":    {`{"results":[{"id":"m1","status":"sent"},{"id":"m0","status":"sent"}]}`, []string{"retry", "retry"}},
		"unknown":      {`{"results":[{"id":"m0","status":"sent"},{"id":"m1","status":"bogus"}]}`, []string{"sent", "retry"}},
		"empty status": {`{"results":[{"id":"m0","status":""},{"id":"m1","status":"rejected"}]}`, []string{"retry", "rejected"}},
		"not json":     {`<html>`, []string{"retry", "retry"}},
		"no results":   {`{}`, []string{"retry", "retry"}},
		"all four":     {`{"results":[{"id":"m0","status":"invalid_token"},{"id":"m1","status":"rejected"}]}`, []string{"invalid_token", "rejected"}},
	} {
		t.Run(name, func(t *testing.T) {
			g := serve(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) })
			res, err := g.Send(context.Background(), twoMsgs())
			if err != nil {
				t.Fatal(err)
			}
			for i, w := range tc.want {
				if res[i].Status != w || res[i].ID != fmt.Sprintf("m%d", i) {
					t.Fatalf("results = %+v, want statuses %v", res, tc.want)
				}
			}
		})
	}
}

func TestGateway_ResponseBodyCapped(t *testing.T) {
	g := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[],"pad":"`)
		io.WriteString(w, strings.Repeat("x", maxResponseBytes+10))
		io.WriteString(w, `"}`)
	})
	if _, err := g.Send(context.Background(), twoMsgs()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestGateway_LocalBatchRules(t *testing.T) {
	var calls int32
	g := serve(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) })
	if _, err := g.Send(context.Background(), nil); !errors.Is(err, ErrBadRequest) {
		t.Errorf("empty: %v", err)
	}
	big := make([]Message, MaxBatch+1)
	for i := range big {
		big[i], _ = NewMessage(BatchID(i), "ios", "t", KindReminder, "n", "c", 1)
	}
	if _, err := g.Send(context.Background(), big); !errors.Is(err, ErrBadRequest) {
		t.Errorf("101: %v", err)
	}
	dup := twoMsgs()
	dup[1].ID = "m0"
	if _, err := g.Send(context.Background(), dup); !errors.Is(err, ErrBadRequest) {
		t.Errorf("duplicate: %v", err)
	}
	if calls != 0 {
		t.Errorf("gateway called %d times for invalid batches", calls)
	}
}

func TestGateway_ErrorsNeverCarrySecretOrToken(t *testing.T) {
	// A transport error: a closed server. url.Error text would carry the URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/?key=" + testSecret
	srv.Close()
	g := NewGateway(url, testSecret)
	_, err := g.Send(context.Background(), twoMsgs())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	for _, leak := range []string{testSecret, "tok-AAA", "tok-BBB"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q contains %q", err, leak)
		}
	}
	// A status error whose body echoes the secret.
	g2 := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, testSecret+" tok-AAA")
	})
	_, err = g2.Send(context.Background(), twoMsgs())
	if err == nil || strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "tok-AAA") {
		t.Fatalf("err = %v", err)
	}
}

func TestBody_FixedAndContentFree(t *testing.T) {
	want := map[string]string{
		KindAssigned: "You were assigned an item",
		KindComment:  "New comment on your item",
		KindReply:    "New reply to your comment",
		KindReminder: "Reminder due",
		KindMention:  "You were mentioned",
		KindSummary:  "You have 7 new notifications",
	}
	for kind, body := range want {
		m, ok := NewMessage("m0", "ios", "t", kind, "nid-1", "cid-1", 7)
		if !ok || m.Body != body || m.Title != "Pad" {
			t.Errorf("%s: %+v ok=%v, want body %q", kind, m, ok, body)
		}
		if m.Data.Kind != kind || m.Data.NID != "nid-1" || m.Data.CID != "cid-1" || m.Data.Count != 7 {
			t.Errorf("%s data = %+v", kind, m.Data)
		}
	}
	if m, _ := NewMessage("m0", "ios", "t", KindMention, "n", "c", 0); m.Data.Count != 1 {
		t.Errorf("count floor: %d", m.Data.Count)
	}
	if _, ok := NewMessage("m0", "ios", "t", "nonsense", "n", "c", 1); ok {
		t.Error("unknown kind built a message")
	}
	if BatchID(0) != "m0" || BatchID(99) != "m99" {
		t.Error("batch ids")
	}
}

// A pin, not a kill (TASK-3939 r4-L5): Send logs nothing and puts no response
// body in its errors today, so this passes by construction. It guards the
// invariant against a future change: no error and no log line, on any failure
// path, carries the secret or a token. Distinctive sentinels make a leak
// unmistakable.
func TestGateway_NoSecretOrTokenInErrorsOrLogs(t *testing.T) {
	const secret, tokA, tokB = "SENTINEL-SECRET-7f3a91", "SENTINEL-TOKEN-A-55d2", "SENTINEL-TOKEN-B-9c04"
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a, _ := NewMessage("m0", "android", tokA, KindAssigned, "n1", "c1", 1)
	b, _ := NewMessage("m1", "ios", tokB, KindMention, "n2", "c2", 1)
	msgs := []Message{a, b}
	echo := secret + " " + tokA + " " + tokB // a hostile gateway echoing everything

	var errs []string
	run := func(name string, g *Gateway) {
		res, err := g.Send(context.Background(), msgs)
		if err != nil {
			errs = append(errs, err.Error())
		}
		for _, r := range res {
			errs = append(errs, r.ID+" "+r.Status)
		}
		_ = name
	}
	status := func(code int) *Gateway {
		return serveWith(t, secret, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, echo)
		})
	}
	run("403", status(403))
	run("503", status(503))
	run("400", status(400))
	run("bad results body", serveWith(t, secret, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, echo) }))
	run("redirect", serveWith(t, secret, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+echo, http.StatusFound)
	}))
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL + "/" + secret + "/" + tokA
	dead.Close()
	run("transport error", NewGateway(url, secret))

	if len(errs) < 6 {
		t.Fatalf("collected %d outputs, want one per path", len(errs))
	}
	all := strings.Join(errs, "\n") + "\n" + logs.String()
	for _, leak := range []string{secret, tokA, tokB} {
		if strings.Contains(all, leak) {
			t.Fatalf("%q leaked into errors or logs:\n%s", leak, all)
		}
	}
}

func serveWith(t *testing.T, secret string, h http.HandlerFunc) *Gateway {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewGateway(srv.URL, secret)
}

// Kills (TASK-3939 r1-H3, r2-F2): closing a non-200 body without draining it.
// Loopback buffers hide an early close from the server, so this asserts the
// observable effect instead: two requests through ONE transport, and the
// second must be served on the first's connection. A body closed unread makes
// the transport drop the connection.
func TestGateway_Non200BodyIsDrained(t *testing.T) {
	// Under the cap, so the bounded drain reaches EOF (TASK-3939 r3-L1).
	body := strings.Repeat("x", maxResponseBytes/4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	g := NewGateway(srv.URL, testSecret)
	g.HTTP = &http.Client{Transport: tr}

	var reused []bool
	for i := 0; i < 2; i++ {
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			reused = append(reused, info.Reused)
		}}
		ctx := httptrace.WithClientTrace(context.Background(), trace)
		if _, err := g.Send(ctx, twoMsgs()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("request %d: err = %v", i, err)
		}
	}
	if len(reused) != 2 || reused[0] || !reused[1] {
		t.Fatalf("GotConnInfo.Reused = %v, want [false true]: the second request did not reuse the connection", reused)
	}
}
