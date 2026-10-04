package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App-owned webhooks (SPEC-6 U10a, TASK-3408). The hook is created from the
// manifest at provisioning and upgrade and is HELD until a redeem hands the
// app its signing secret; delivery (U10b) skips a held hook.

// appWebhookHeldNotice is what the owner sees while the hook waits for its
// secret. The reissue is POST .../apps/{installID}/install-code.
const appWebhookHeldNotice = "This app's webhook needs a new install code to activate: issue one and give it to the app."

// appWebhookSpec translates the manifest's events (by companion KEY) into the
// store's spec (by companion SLUG). nil when the manifest declares no events.
func appWebhookSpec(m *appmanifest.Manifest) *store.AppWebhookSpec {
	if m == nil || len(m.Events) == 0 || m.WebhookURL == "" {
		return nil
	}
	slugByKey := map[string]string{}
	for _, c := range m.CompanionPack.Collections {
		slugByKey[c.Key] = c.Slug
	}
	spec := &store.AppWebhookSpec{URL: m.WebhookURL}
	for _, e := range m.Events {
		ev := store.AppWebhookEvent{Name: e.Name}
		for _, k := range e.Collections {
			// The manifest validator refused a key that is not a companion's.
			ev.CollectionSlugs = append(ev.CollectionSlugs, slugByKey[k])
		}
		spec.Events = append(spec.Events, ev)
	}
	return spec
}

// appWebhookSpecFromJSON is appWebhookSpec over a stored manifest.
func appWebhookSpecFromJSON(manifestJSON string) (*store.AppWebhookSpec, error) {
	var m appmanifest.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		return nil, err
	}
	return appWebhookSpec(&m), nil
}

// appWebhookView is the owner-facing hook status, with the held notice.
type appWebhookView struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	Notice string `json:"notice,omitempty"`
}

func (s *Server) appWebhookView(installID string) (*appWebhookView, error) {
	st, err := s.store.GetAppWebhookStatus(installID)
	if err != nil || st == nil {
		return nil, err
	}
	v := &appWebhookView{URL: st.URL, Status: st.Status}
	if st.Status == store.AppWebhookAwaitingSecret {
		v.Notice = appWebhookHeldNotice
	}
	return v, nil
}

// GET /workspaces/{ws}/apps/{installID}: the install's state and its hook.
func (s *Server) handleGetAppInstall(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	state, err := s.store.InstallState(workspaceID, installID)
	if err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	out := appInstallStateResponse{InstallID: installID, State: state}
	if out.Webhook, err = s.appWebhookView(installID); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// EnsureAppWebhooks backfills the hook of every active or inactive install
// provisioned before U10 whose manifest declares one. Each is created HELD:
// the app was never given a secret and gets one at its next redeem. Run at
// server start; idempotent (an install that has a hook is skipped under its
// row lock). A failure is logged per install and never stops the start.
func (s *Server) EnsureAppWebhooks(ctx context.Context) {
	list, err := s.store.ListInstallsWithoutWebhook()
	if err != nil {
		slog.Error("apps: webhook backfill: list", "error", err)
		return
	}
	for _, in := range list {
		if ctx.Err() != nil {
			return
		}
		if err := s.store.EnsureAppWebhook(in.WorkspaceID, in.InstallID, appWebhookSpecFromJSON); err != nil {
			slog.Error("apps: webhook backfill", "install_id", in.InstallID, "error", err)
		}
	}
}
