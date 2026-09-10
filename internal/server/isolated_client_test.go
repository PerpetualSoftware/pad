package server

import "net/http"

// isolatedTestClient returns an HTTP client with a transport of its own, for
// tests that must survive another test's teardown (BUG-3008).
//
// WHY THIS EXISTS. `httptest.Server.Close()` closes idle connections on the
// PROCESS-WIDE default transport — the standard library says so in its own
// comment, calling it "not part of httptest.Server's correctness" and doing it
// to help out users who are on the standard transport:
//
//	if t, ok := http.DefaultTransport.(closeIdleTransport); ok {
//	    t.CloseIdleConnections()
//	}
//
// So in a package where several PARALLEL tests each stand up an httptest
// server, any one of them finishing can break an in-flight request another one
// is making through `http.DefaultClient`. The symptom is
// "transport connection broken: http: CloseIdleConnections called" on a request
// that had nothing to do with the server that closed, and it reproduces only
// when two tests interleave inside a window of microseconds — so it reads as
// infrastructure noise and survives local `-count` runs.
//
// A client with its own transport cannot be reached that way.
//
// It is deliberately not a shared package-level var: two tests holding one
// transport would reintroduce a smaller version of the same coupling.
func isolatedTestClient() *http.Client {
	// No Timeout on purpose. These are SSE and long-poll requests; a timeout
	// here would cancel the stream the test is measuring.
	return &http.Client{Transport: &http.Transport{}}
}
