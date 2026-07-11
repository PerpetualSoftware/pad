package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// resolveShareWithPassword issues an anonymous GET /api/v1/s/{token} from the
// given source IP with the password supplied via the X-Share-Password header
// (the only accepted channel — see handleResolveShareLink).
func resolveShareWithPassword(srv *Server, token, password, remoteIP string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/s/"+token, nil)
	req.Header.Set("X-Share-Password", password)
	req.RemoteAddr = remoteIP + ":1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TestResolveShareLink_PasswordBruteForceLimiter pins TASK-2055: password
// verification on a share link must be throttled per-share so a
// password-protected link can't be ground offline-fast. The limiter is
// charged before the bcrypt compare, so a correct password still works while
// there's burst budget, and once the burst is exhausted further attempts
// (right or wrong) get a 429.
func TestResolveShareLink_PasswordBruteForceLimiter(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	// Create the collection BEFORE any user exists — the first user flips the
	// instance into auth-required mode, and the public unauthenticated
	// doRequest helper would then be CSRF-rejected. A real collection lets the
	// correct-password path resolve to a clean 200.
	cr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Shared",
		"prefix": "SHAR",
	})
	if cr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", cr.Code, cr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, cr, &coll)

	// Mint a password-protected share link directly via the store. The public
	// resolve path is what we're exercising; a real created_by user satisfies
	// the FK without standing up an auth session.
	owner, err := srv.store.CreateUser(models.UserCreate{Email: "bf-owner@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}

	const correct = "correct-horse-battery-staple"
	link, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, &store.ShareLinkOptions{
		Password: correct,
	})
	if err != nil {
		t.Fatalf("create share link: %v", err)
	}
	if !link.HasPassword {
		t.Fatal("expected share link to require a password")
	}

	const attackerIP = "192.0.2.10"
	const viewerIP = "192.0.2.20"

	// Per-IP burst is 5 (see RateLimiters.SharePasswordIP). The first attempt
	// uses the correct password and must succeed — a correct password works
	// while there's budget.
	rr := resolveShareWithPassword(srv, link.Token, correct, attackerIP)
	if rr.Code != http.StatusOK {
		t.Fatalf("correct password within budget: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Remaining budget for this IP is 4. Fire 4 wrong-password attempts — each
	// clears the limiter (403 Incorrect password), not throttled yet.
	for i := 0; i < 4; i++ {
		rr := resolveShareWithPassword(srv, link.Token, "wrong-guess", attackerIP)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("wrong attempt %d within budget: expected 403, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}

	// Budget is now exhausted for this IP (1 correct + 4 wrong = 5). The next
	// attempt is rate-limited regardless of the password supplied.
	rr = resolveShareWithPassword(srv, link.Token, "wrong-guess", attackerIP)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt past burst: expected 429, got %d: %s", rr.Code, rr.Body.String())
	}
	// Even a correct password is throttled once the burst is spent — the
	// limiter is charged before the compare, which is what forces an attacker
	// offline-fast grind into an online-slow crawl.
	rr = resolveShareWithPassword(srv, link.Token, correct, attackerIP)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password past burst: expected 429, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on rate-limit response")
	}

	// No cross-viewer lockout: the key folds in the client IP, so a DIFFERENT
	// viewer of the SAME link is unaffected by the attacker exhausting their
	// own bucket. A per-share-only key would 429 this legitimate viewer.
	rr = resolveShareWithPassword(srv, link.Token, correct, viewerIP)
	if rr.Code != http.StatusOK {
		t.Fatalf("second viewer of same link must have its own budget, got %d: %s", rr.Code, rr.Body.String())
	}

	// A DIFFERENT share link also has its own bucket (per-share keying).
	other, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, &store.ShareLinkOptions{
		Password: correct,
	})
	if err != nil {
		t.Fatalf("create second share link: %v", err)
	}
	rr = resolveShareWithPassword(srv, other.Token, correct, attackerIP)
	if rr.Code != http.StatusOK {
		t.Fatalf("second share link must have its own budget, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestResolveShareLink_AggregateCap pins the anti-botnet layer of TASK-2055: a
// distributed attacker rotating source IPs gets a fresh per-IP burst from each
// address, so the per-share aggregate bucket must cap link-wide guessing. It's
// charged before the password compare, so once exhausted even a would-be-
// correct guess is blocked — no password oracle survives the link-wide cap.
func TestResolveShareLink_AggregateCap(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	cr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Shared",
		"prefix": "SHAR",
	})
	if cr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", cr.Code, cr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, cr, &coll)

	owner, err := srv.store.CreateUser(models.UserCreate{Email: "agg-owner@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}

	const correct = "correct-horse-battery-staple"
	link, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, &store.ShareLinkOptions{
		Password: correct,
	})
	if err != nil {
		t.Fatalf("create share link: %v", err)
	}

	// Aggregate burst is 60 (see RateLimiters.SharePasswordShare). Keep each IP
	// to 5 wrong guesses so the per-IP bucket (burst 5) never trips — that way
	// any 429 we see is unambiguously the link-wide layer. ~12 distinct IPs ×
	// 5 = 60 exhausts it; keep going until a 429 appears.
	sawThrottle := false
	total := 0
outer:
	for ipN := 0; ipN < 20; ipN++ {
		ip := "198.51.100." + strconv.Itoa(ipN+1)
		for a := 0; a < 5; a++ {
			rr := resolveShareWithPassword(srv, link.Token, "wrong-guess", ip)
			if rr.Code == http.StatusTooManyRequests {
				sawThrottle = true
				break outer
			}
			if rr.Code != http.StatusForbidden {
				t.Fatalf("wrong guess from %s: expected 403, got %d: %s", ip, rr.Code, rr.Body.String())
			}
			total++
		}
	}
	if !sawThrottle {
		t.Fatal("aggregate cap never engaged across distinct-IP wrong guesses")
	}
	if total < 60 {
		t.Fatalf("aggregate cap tripped too early after %d guesses (burst is 60)", total)
	}

	// No password oracle: with the link-wide budget spent, even a CORRECT guess
	// from a brand-new IP is blocked pre-validation (429), so an attacker can't
	// keep probing candidates and read success/failure once the cap engages.
	rr := resolveShareWithPassword(srv, link.Token, correct, "203.0.113.7")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("correct guess past aggregate cap must be blocked (no oracle), got %d: %s", rr.Code, rr.Body.String())
	}
}
