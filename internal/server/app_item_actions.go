package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App item actions and context codes (SPEC-6 U11, DOC-3371 §6; TASK-3414).
//
//   GET  /api/v1/workspaces/{ws}/items/{ref}/app-actions
//        the actions offered on an item, for the item pane's button;
//   POST /api/v1/workspaces/{ws}/items/{ref}/app-actions/{installID}/{actionKey}
//        mints a context code for the caller and answers {url};
//   POST /api/app/v1/workspaces/{ws}/context/redeem {code}
//        the app redeems it with its service token and receives the item.
//
// Mint and redeem refuse with ONE 404 body for every reason, so neither is
// an oracle for installs, actions, items or codes.

// writeContextRefused is the single refusal of a mint or a redeem.
func writeContextRefused(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "Not found")
}

// appActionSpecs translates the manifest's item actions (by companion KEY)
// into the store's specs (by companion SLUG).
func appActionSpecs(m *appmanifest.Manifest) []store.AppActionSpec {
	if m == nil || len(m.ItemActions) == 0 {
		return nil
	}
	slugByKey := map[string]string{}
	for _, c := range m.CompanionPack.Collections {
		slugByKey[c.Key] = c.Slug
	}
	out := make([]store.AppActionSpec, 0, len(m.ItemActions))
	for _, a := range m.ItemActions {
		spec := store.AppActionSpec{Key: a.Key, Label: a.Label, Path: a.Path}
		for _, k := range a.Collections {
			// The manifest validator refused a key that is not a companion's.
			spec.CollectionSlugs = append(spec.CollectionSlugs, slugByKey[k])
		}
		out = append(out, spec)
	}
	return out
}

func appActionSpecsFromJSON(manifestJSON string) ([]store.AppActionSpec, error) {
	var m appmanifest.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		return nil, err
	}
	return appActionSpecs(&m), nil
}

// EnsureAppItemActions backfills the action rows of installs provisioned
// before U11 (the EnsureAppWebhooks pattern). Idempotent; a failure is
// logged per install and never stops the start.
func (s *Server) EnsureAppItemActions(ctx context.Context) {
	list, err := s.store.ListInstallsWithoutActions()
	if err != nil {
		slog.Error("apps: item action backfill: list", "error", err)
		return
	}
	for _, in := range list {
		if ctx.Err() != nil {
			return
		}
		if err := s.store.EnsureAppItemActions(in.WorkspaceID, in.InstallID, appActionSpecsFromJSON); err != nil {
			slog.Error("apps: item action backfill", "install_id", in.InstallID, "error", err)
		}
	}
}

// contextViewerVisible is the viewer re-admission for mint and redeem: the
// human item read's own visibility rule (checkItemVisibleQ) with the
// viewer's CURRENT role, memberships and grants (lead ruling, day 88). The
// role is crossWorkspaceRole's for a bearer caller (store.ViewerRoleQ), so a
// platform admin who is not a member does not pass on the admin cookie
// bypass: an app is about to receive the item.
func (s *Server) contextViewerVisible() store.ContextVisibleFunc {
	return func(q store.Queryer, item *models.Item, viewerID string) (bool, error) {
		// Every read on q, the mint or redeem's own transaction: a pool read
		// here would wait for a second connection while holding the first,
		// and under load every connection can be held that way (codex r1 on
		// U11).
		viewer, err := s.store.GetUserQ(q, viewerID)
		if err != nil {
			return false, err
		}
		if viewer == nil || viewer.IsDisabled() || viewer.Kind == models.UserKindApp {
			return false, nil
		}
		role, err := s.store.ViewerRoleQ(q, item.WorkspaceID, viewer)
		if err != nil {
			return false, err
		}
		if role == "" {
			return false, nil
		}
		return s.checkItemVisibleQ(q, item.WorkspaceID, item, viewer, role, true)
	}
}

// handleListItemAppActions: GET …/items/{ref}/app-actions.
func (s *Server) handleListItemAppActions(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item, err := s.store.ResolveItem(workspaceID, chi.URLParam(r, "itemSlug"))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	actions, err := s.store.ListItemAppActions(workspaceID, item.CollectionID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

// handleMintItemAppAction: POST …/items/{ref}/app-actions/{installID}/{actionKey}.
// The caller is the viewer. The mint re-checks, under the redeem's locks,
// everything a redeem will (lead ruling, day 88).
func (s *Server) handleMintItemAppAction(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	viewer := currentUser(r)
	if viewer == nil {
		writeContextRefused(w)
		return
	}
	item, err := s.store.ResolveItem(workspaceID, chi.URLParam(r, "itemSlug"))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeContextRefused(w)
		return
	}
	minted, err := s.store.MintContextCode(workspaceID, chi.URLParam(r, "installID"), chi.URLParam(r, "actionKey"),
		item.ID, viewer.ID, s.contextViewerVisible())
	if errors.Is(err, store.ErrContextCodeRefused) {
		writeContextRefused(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	target, err := contextURL(minted.BaseURL, minted.Path, minted.Code)
	if err != nil {
		writeContextRefused(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"url": target})
}

// contextURL is {base_url}{path} with pad_context added to the path's own
// query, if it has one.
func contextURL(base, path, code string) (string, error) {
	u, err := url.Parse(base + path)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("pad_context", code)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// appContextRedeemMaxBody bounds the redeem body: one short code.
const appContextRedeemMaxBody = 4 << 10

// AppContextRedeemed is the redeem answer: the action and the item, as the
// app API's item DTO. Never the viewer (§6: the code names an item).
type AppContextRedeemed struct {
	ActionKey string  `json:"action_key"`
	Item      AppItem `json:"item"`
}

// appRedeemContext: POST /api/app/v1/workspaces/{ws}/context/redeem.
func (s *Server) appRedeemContext(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeJSONWithLimit(r, &in, appContextRedeemMaxBody); err != nil || in.Code == "" {
		writeContextRefused(w)
		return
	}
	// §6: a service token only (the route table's Auth column is
	// documentation; this is the check). A refused attempt still consumes
	// the code, as every refusal does (codex r2 on U11).
	if ac.AuthKind != "service" {
		if err := s.store.BurnContextCode(ac.InstallID, in.Code); err != nil {
			writeInternalError(w, err)
			return
		}
		writeContextRefused(w)
		return
	}
	red, err := s.store.RedeemContextCode(ac.InstallID, ac.Grant.AuthEpoch, in.Code, s.contextViewerVisible())
	if errors.Is(err, store.ErrContextCodeRefused) {
		writeContextRefused(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// The ceiling, as on every app read: the redeem already required a
	// current companion, so this refuses only if the token's ceiling and the
	// install's companions disagree.
	if red.Item.WorkspaceID != ac.WorkspaceID || !appCeilingAllows(r, red.Item.CollectionID) {
		writeContextRefused(w)
		return
	}
	coll, err := s.store.GetCollection(red.Item.CollectionID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeContextRefused(w)
		return
	}
	dtos, err := s.appItemDTOs(r, []models.Item{*red.Item}, coll)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeAppJSON(w, http.StatusOK, AppContextRedeemed{ActionKey: red.ActionKey, Item: dtos[0]})
}
