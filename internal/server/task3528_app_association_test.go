package server

import (
	"net/http"
	"strings"
	"testing"
)

// TASK-3528: Android's assetlinks.json and Apple's app-site-association name
// the official Pad app, so Pad Cloud's edge router serves them (pad-cloud's
// nginx-router.conf and ./well-known) and the binary never does. This pins the
// binary's half: on a self-hosted install AND in cloud mode, both paths answer
// the JSON 404 every unpublished /.well-known document gets (TASK-3321 U0a).
// A route added here later would claim the official app's credentials for
// every self-hosted domain, and in cloud mode would shadow nothing (the router
// answers first) while still shipping the coupling in the public binary.
func TestTASK3528_BinaryServesNoAppAssociation(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/.well-known/assetlinks.json",
		"/.well-known/apple-app-site-association",
	}
	spa := webFS(map[string]string{"index.html": "<!doctype html><html></html>"})

	selfHosted := testServer(t)
	selfHosted.SetWebUI(spa)
	cloud := testServer(t)
	cloud.SetCloudMode("test-secret")
	cloud.SetWebUI(spa)

	for name, srv := range map[string]*Server{"self-hosted": selfHosted, "cloud": cloud} {
		for _, p := range paths {
			rr := doRequest(srv, "GET", p, nil)
			if rr.Code != http.StatusNotFound {
				t.Errorf("%s: GET %s = %d, want 404", name, p, rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("%s: GET %s Content-Type = %q, want the JSON 404 (application/json)", name, p, ct)
			}
			if strings.Contains(rr.Body.String(), "perpetualsoftware") {
				t.Errorf("%s: GET %s names the official app: %s", name, p, rr.Body.String())
			}
		}
	}
}
