package redisdial

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

// tlsServerFor starts a TLS listener whose certificate is valid for the given
// names, and returns its address plus a config that trusts it.
func tlsServerFor(t *testing.T, names ...string) (addr string, trust *tls.Config) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	// IP literals belong in IPAddresses, not DNSNames — an IP SAN is a
	// different field and x509 will not match one against the other.
	var dnsNames []string
	var ips []net.IP
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, n)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// Drive the handshake, then hold the connection so the client
				// side sees a completed one.
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
			}()
		}
	}()
	return ln.Addr().String(), &tls.Config{RootCAs: pool}
}

// TestAWrongNamedCertificateIsRefused is the ServerName assertion, and it is
// deliberately NOT "the field is set".
//
// The hazard this whole dialer has to avoid is turning a latency fix into a
// silent authentication regression: tls.DialWithDialer infers ServerName from
// the dialled address when the config leaves it empty, and a hand-rolled
// tls.Client does not. An empty ServerName leaves verification with no name to
// check against. Asserting that a field is populated proves the code sets a
// field; connecting to a certificate issued for the WRONG name and requiring
// refusal proves the property.
func TestAWrongNamedCertificateIsRefused(t *testing.T) {
	addr, trust := tlsServerFor(t, "the-right-name.example")

	// ServerName deliberately EMPTY, so the fallback is what supplies it. The
	// dialled address is 127.0.0.1, which the certificate does not cover.
	dial := New(trust, 5*time.Second)

	conn, err := dial(context.Background(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a certificate issued for another name was accepted: with no ServerName the dialer has nothing to verify against, " +
			"and this fix would have converted certificate verification into a no-op")
	}
	var unknown x509.HostnameError
	if !errors.As(err, &unknown) {
		t.Fatalf("refused for the wrong reason (%v); want a hostname mismatch, which is what proves the name was actually checked", err)
	}
}

// TestAMatchingCertificateIsAccepted is the control. Without it, "wrong names
// are refused" is satisfied by a dialer that refuses everything — which would
// pass the test above while breaking every TLS deployment.
func TestAMatchingCertificateIsAccepted(t *testing.T) {
	addr, trust := tlsServerFor(t, "127.0.0.1")

	conn, err := dialWithServerName(t, trust, addr, "")
	if err != nil {
		t.Fatalf("a certificate valid for the dialled host was refused: %v", err)
	}
	_ = conn.Close()
}

// TestAnExplicitServerNameIsHonoured pins that the fallback does not OVERWRITE
// a configured name — which is the case Pad actually runs, since
// redis.ParseURL populates ServerName for a rediss:// URL.
func TestAnExplicitServerNameIsHonoured(t *testing.T) {
	addr, trust := tlsServerFor(t, "configured.example")

	conn, err := dialWithServerName(t, trust, addr, "configured.example")
	if err != nil {
		t.Fatalf("an explicitly configured ServerName was not honoured: %v", err)
	}
	_ = conn.Close()
}

// TestTheCallersConfigIsNotMutated is the reason the fallback clones. A shared
// tls.Config is dialled through many times; writing the first host's name into
// it would make every later dial verify against the wrong name — and would do
// so only in production, where one config is reused.
func TestTheCallersConfigIsNotMutated(t *testing.T) {
	addr, trust := tlsServerFor(t, "127.0.0.1")

	dial := New(trust, 5*time.Second)
	conn, err := dial(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	if trust.ServerName != "" {
		t.Fatalf("the caller's config was mutated (ServerName = %q): a shared config would carry one host's name into every later dial",
			trust.ServerName)
	}
}

func dialWithServerName(t *testing.T, trust *tls.Config, addr, serverName string) (net.Conn, error) {
	t.Helper()
	cfg := trust.Clone()
	cfg.ServerName = serverName
	return New(cfg, 5*time.Second)(context.Background(), "tcp", addr)
}

// stalledTLSServer accepts TCP connections and then says nothing. The TCP
// connect succeeds; the handshake never completes.
//
// THIS IS THE SHAPE THE DEFECT AND ITS FIX BOTH LIVE IN. A dial bounded only
// at the connect looks healthy here and then hangs for as long as whoever is
// waiting is prepared to wait.
func stalledTLSServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // accepted, never answered
		}
	}()
	return ln.Addr().String()
}

// TestACancelledCallerDoesNotWaitOutATLSHandshake is BUG-2754's regression
// test, and the whole reason this package exists.
//
// go-redis's default dialer hands TLS connections to tls.DialWithDialer, which
// takes NO context — so a cancelled caller could not shorten the dial and paid
// DialTimeout in full. On the SSE path that is an admission slot, global and
// per-user, held for a client that has already gone.
//
// Asserted against DialTimeout rather than against a wall-clock number, so it
// fails against the unfixed behaviour whatever that constant is set to.
func TestACancelledCallerDoesNotWaitOutATLSHandshake(t *testing.T) {
	addr := stalledTLSServer(t)
	const dialTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	// BOUNDED, for the same reason its sibling below is: against the unfixed
	// dialer this never returns, and a hung test run is not a detection anyone
	// can act on — it also strands the mutation harness with its edit applied,
	// which happened here once.
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := New(&tls.Config{InsecureSkipVerify: true}, dialTimeout)(ctx, "tcp", addr) //nolint:gosec // the handshake never completes; verification is not what is under test
		done <- result{err, time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("a cancelled caller had not returned after 3s: cancellation cannot reach the handshake, so it will wait "+
			"out the full DialTimeout of %v", dialTimeout)
	}
	err, elapsed := got.err, got.elapsed

	if err == nil {
		t.Fatal("a handshake against a server that never answers must not succeed")
	}
	if elapsed >= dialTimeout {
		t.Fatalf("a cancelled caller waited %v, the full DialTimeout of %v: cancellation cannot reach the handshake", elapsed, dialTimeout)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("failed with %v; want context.Canceled, which is what proves the CALLER ended it rather than a timeout", err)
	}
}

// TestTheTimeoutStillBoundsAStalledHandshake is the other half, and the
// direction that is easy to lose while fixing the first.
//
// If the timeout bounded only the TCP connect, a server that accepts and then
// stalls would hang for as long as the CONTEXT lives — trading a bounded
// failure for an unbounded one, which is worse than the bug being fixed. The
// caller here never cancels.
func TestTheTimeoutStillBoundsAStalledHandshake(t *testing.T) {
	addr := stalledTLSServer(t)
	const dialTimeout = 150 * time.Millisecond

	// BOUNDED SO A HANG BECOMES A FAILURE. Without the fix this dial never
	// returns at all, and an unbounded wait turns the detection into a hung
	// test run — which is not a result anyone can act on, and which strands
	// the mutation harness with its edit still applied.
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := New(&tls.Config{InsecureSkipVerify: true}, dialTimeout)(context.Background(), "tcp", addr) //nolint:gosec // as above
		done <- result{err, time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("a stalled handshake had not returned after 3s with a %v timeout: the bound covers the connect but not "+
			"the handshake, so an unresponsive TLS server hangs for as long as the context lives", dialTimeout)
	}
	err, elapsed := got.err, got.elapsed

	if err == nil {
		t.Fatal("a handshake against a server that never answers must not succeed")
	}
	if elapsed > time.Second {
		t.Fatalf("a stalled handshake took %v with a %v timeout: the bound is not being applied to the handshake",
			elapsed, dialTimeout)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed with %v; want context.DeadlineExceeded, which is what proves the TIMEOUT ended it", err)
	}
}
