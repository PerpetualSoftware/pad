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
// server, any one of them finishing reaches into a transport every other test
// is sharing. `Transport.CloseIdleConnections` does three things there, and the
// first two can land on a request that is already under way:
//
//   - it closes every pooled connection with `errCloseIdleConns`, which a
//     concurrent `getConn` may have just handed to a request;
//   - it cancels the dials in progress that are not waiting
//     (`w.cancelCtx()` over `t.dialsInProgress`);
//   - it sets `closeIdle`, so connections that go idle later close too.
//
// The symptom is "transport connection broken: http: CloseIdleConnections
// called" on a request that had nothing to do with the server that closed. It
// needs two tests to interleave inside a window of microseconds, so it reads as
// infrastructure noise and survives local `-count` runs.
//
// A client with its own transport cannot be reached that way.
//
// It is deliberately not a shared package-level var: two tests holding one
// transport would reintroduce a smaller version of the same coupling. The
// per-call transport does not accumulate anything, either — `httptest.Server.Close`
// waits for outstanding requests and closes every connection it accepted, so
// the client's pool is dead by the time the test returns.
func isolatedTestClient() *http.Client {
	// No Timeout on purpose. These are SSE and long-poll requests; a timeout
	// here would cancel the stream the test is measuring.
	return &http.Client{Transport: &http.Transport{}}
}
