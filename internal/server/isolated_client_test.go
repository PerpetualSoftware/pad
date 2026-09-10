package server

import "net/http"

// isolatedTestClient returns an HTTP client with a transport of its own. Test
// requests in this package go through it (or through srv.Client()), never
// through http.DefaultClient (BUG-3008).
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
// `Transport.CloseIdleConnections` closes each pooled connection with
// `errCloseIdleConns` — the error in that message — and sets `closeIdle`, so
// connections going idle afterwards are closed instead of pooled until the next
// `queueForIdleConn` clears the flag. It also cancels dials in progress that
// are not waiting. Its doc comment states that it "does not interrupt any
// connections currently in use", and the idle pool is mutated under `idleMu`,
// so the precise interleaving by which that error reached a caller's `Do()` is
// NOT reconstructed here.
//
// That unknown is the argument for isolation rather than a narrower repair: a
// client with its own transport is outside the reach of another test's
// teardown whichever window it was. Guessing at the window and fixing only
// that would leave the next seat to rediscover this from a worse position.
//
// It is deliberately not a shared package-level var: two tests holding one
// transport would reintroduce a smaller version of the same coupling.
func isolatedTestClient() *http.Client {
	// No Timeout on purpose. These are SSE and long-poll requests; a timeout
	// here would cancel the stream the test is measuring.
	return &http.Client{Transport: &http.Transport{}}
}
