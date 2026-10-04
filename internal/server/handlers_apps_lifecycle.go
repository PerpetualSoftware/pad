package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App install lifecycle doors (SPEC-6 U8c, DOC-3371 §2 and §8; TASK-3397).
// Owner-only, behind appsAvailable. Each door runs phase 1, the delivery
// drain and phase 2 in order; repeating a call on an install left between the
// phases (a crash) resumes it.
//
// POST /workspaces/{ws}/apps/{installID}/disable    active -> inactive
// POST /workspaces/{ws}/apps/{installID}/enable     inactive -> active
// POST /workspaces/{ws}/apps/{installID}/rotate     active -> active, new code
// POST /workspaces/{ws}/apps/{installID}/uninstall  -> uninstalled (tombstone)

type appInstallStateResponse struct {
	InstallID   string `json:"install_id"`
	State       string `json:"state"`
	InstallCode string `json:"install_code,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Notice      string `json:"notice,omitempty"`
	// Webhook is the install's hook, absent when it has none (U10a).
	Webhook *appWebhookView `json:"webhook,omitempty"`
}

func (s *Server) writeInstallLifecycleError(w http.ResponseWriter, err error) {
	var se *store.InstallStateError
	switch {
	case errors.Is(err, store.ErrInstallNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Install not found")
	case errors.As(err, &se):
		writeError2(w, http.StatusConflict, "install_state", "The install is "+se.State+"; this action does not apply", map[string]interface{}{"state": se.State})
	default:
		writeInternalError(w, err)
	}
}

// writeDrainError answers a drain that did not finish. The install is left
// between the phases and repeating the same call resumes it.
func (s *Server) writeDrainError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrDrainTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "deliveries_in_flight", "The app still has webhook deliveries in flight; repeat this request to finish")
		return
	}
	writeInternalError(w, err)
}

func (s *Server) installLifecycleDone(w http.ResponseWriter, r *http.Request, workspaceID, installID, action string, extra func(*appInstallStateResponse)) {
	state, err := s.store.InstallState(workspaceID, installID)
	if err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	s.logAuditEvent(models.ActionAppInstallLifecycle, r, auditMeta(map[string]string{
		"workspace_id": workspaceID, "install_id": installID, "action": action, "state": state,
	}))
	out := appInstallStateResponse{InstallID: installID, State: state}
	if out.Webhook, err = s.appWebhookView(installID); err != nil {
		writeInternalError(w, err)
		return
	}
	if extra != nil {
		extra(&out)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDisableAppInstall(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	if err := s.store.BeginInstallTeardown(workspaceID, installID, store.TeardownDisable); err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	if err := s.store.DrainInstallDeliveries(r.Context(), installID); err != nil {
		s.writeDrainError(w, err)
		return
	}
	if err := s.store.FinishDisable(workspaceID, installID); err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	s.installLifecycleDone(w, r, workspaceID, installID, "disable", nil)
}

func (s *Server) handleEnableAppInstall(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	if err := s.store.ReenableInstall(workspaceID, installID); err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	s.installLifecycleDone(w, r, workspaceID, installID, "enable", func(o *appInstallStateResponse) {
		o.Notice = "Re-enabled. Tokens issued before the disable stay revoked; the app obtains new ones."
	})
}

func (s *Server) handleRotateAppInstall(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	if err := s.store.BeginInstallTeardown(workspaceID, installID, store.TeardownRotate); err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	if err := s.store.DrainInstallDeliveries(r.Context(), installID); err != nil {
		s.writeDrainError(w, err)
		return
	}
	code, exp, err := s.store.FinishRotate(workspaceID, installID)
	if err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.installLifecycleDone(w, r, workspaceID, installID, "rotate", func(o *appInstallStateResponse) {
		o.InstallCode, o.ExpiresAt, o.Notice = code, exp.UTC().Format("2006-01-02T15:04:05Z"), appInstallCodeNotice
	})
}

func (s *Server) handleUninstallAppInstall(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	if err := s.store.BeginInstallTeardown(workspaceID, installID, store.TeardownUninstall); err != nil {
		var se *store.InstallStateError
		if !errors.As(err, &se) || se.State != store.InstallUninstalled {
			s.writeInstallLifecycleError(w, err)
			return
		}
		// Already a tombstone: answer its state.
	}
	if err := s.store.DrainInstallDeliveries(r.Context(), installID); err != nil {
		s.writeDrainError(w, err)
		return
	}
	if err := s.store.UninstallAppTx(workspaceID, installID); err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	s.installLifecycleDone(w, r, workspaceID, installID, "uninstall", nil)
}
