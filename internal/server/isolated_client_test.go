package server

import "net/http"

// isolatedTestClient returns an HTTP client with a transport of its own.
// Outbound requests in this package's tests go through it — or through the
// httptest server's own `ts.Client()` where the test holds the server value —
// and never through `http.DefaultClient`, `http.Get`, or an `http.Client`
// literal with a nil Transport, which is `http.DefaultTransport` (BUG-3008).
//
// WHY THIS EXISTS. `httptest.Server.Close()` reaches into the PROCESS-WIDE
// default transport — the standard library says so in its own comment, calling
// it "not part of httptest.Server's correctness" and doing it to help out
// users who are on the standard transport:
//
//	if t, ok := http.DefaultTransport.(closeIdleTransport); ok {
//	    t.CloseIdleConnections()
//	}
//
// So in a package where many tests each stand up an httptest server, every
// `defer ts.Close()` mutates state that every other test's requests depend on.
// What CI observed (BUG-2949, then BUG-3008) is a request in one test failing
// with "transport connection broken: http: CloseIdleConnections called" while
// the server that closed belonged to a different test entirely.
//
// WHAT IS AND IS NOT CLAIMED HERE. Read out of `net/http/transport.go`,
// `Transport.CloseIdleConnections` closes each pooled HTTP/1 connection with
// `errCloseIdleConns` — the error in that message; HTTP/2 connections go
// through `h2transport.CloseIdleConnections()` separately. It sets
// `closeIdle`, so connections going idle afterwards are closed instead of
// pooled, until a later `queueForIdleConn` clears it (which it does not reach
// when `DisableKeepAlives` is set). It also cancels the dials in progress that
// have a `cancelCtx` and are not waiting.
//
// Its doc comment states that it "does not interrupt any connections currently
// in use", and the idle pool is mutated under `idleMu`, so the precise
// interleaving by which that error reached a caller's `Do()` is NOT
// reconstructed here.
//
// That unknown is the argument for isolation rather than a narrower repair: a
// client with its own transport is outside the reach of another test's
// teardown whichever window it was. Guessing at the window and fixing only
// that would leave the next seat to rediscover this from a worse position.
//
// It is deliberately not a shared package-level var: two tests holding one
// transport would reintroduce a smaller version of the same coupling.
//
// NOT A GENERAL RULE FOR EVERY PACKAGE. `internal/cli`'s tests drive the
// PRODUCTION client from `NewClientFromURL`, which has a nil Transport by
// design, so its sends cannot be isolated without changing what the shipped
// CLI does. There the boundary really is parallel-vs-serial: the two
// `t.Parallel()` tests that build their own requests use `srv.Client()`, and
// the rest are serial, which a parallel test's teardown cannot overlap.
func isolatedTestClient() *http.Client {
	// No Timeout on purpose. These are SSE and long-poll requests; a timeout
	// here would cancel the stream the test is measuring. A caller that wants
	// one sets it on the returned client.
	return &http.Client{Transport: &http.Transport{}}
}
