package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App install provisioning, install codes and redeem (SPEC-6 U8b, DOC-3371
// §2 steps 6-8, §3, §4 Credentials; TASK-3397).
//
// POST /workspaces/{ws}/apps/install/pending/{pendingID}/confirm {manifest_sha256}
//   re-normalize from the staged bytes, compare with the reviewed digests,
//   then provision in one store transaction that re-checks every
//   state-dependent fact. Answers {install_id, install_code, expires_at}.
// POST /workspaces/{ws}/apps/{installID}/install-code
//   a new install code for an active install (recovery for a lost redeem).
// POST /api/app/v1/install/redeem {code}
//   unauthenticated; consumes the code, rotates the client secret and returns
//   {install_id, client_id, client_secret} exactly once.

// appRedeemMaxBody is the redeem route's body cap.
const appRedeemMaxBody = 4 << 10

type appInstallConfirmResponse struct {
	InstallID   string        `json:"install_id"`
	InstallCode string        `json:"install_code"`
	ExpiresAt   time.Time     `json:"expires_at"`
	Notice      string        `json:"notice"`
	Items       []confirmItem `json:"items"`
}

type confirmItem struct {
	Key    string `json:"key"`
	Ref    string `json:"ref"`
	Status string `json:"status,omitempty"`
}

const appInstallCodeNotice = "Give this install code to the app. It works once, for 10 minutes. If the app loses it, issue a new code from the install."

func (s *Server) handleConfirmAppInstall(w http.ResponseWriter, r *http.Request) {
	workspaceID, owner, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	var in struct {
		ManifestSHA256 string `json:"manifest_sha256"`
	}
	if err := decodeJSON(r, &in); err != nil || in.ManifestSHA256 == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Body must be {\"manifest_sha256\": \"<the hash you reviewed>\"}")
		return
	}
	p, err := s.store.GetPendingInstall(chi.URLParam(r, "pendingID"), workspaceID, owner.ID)
	if errors.Is(err, store.ErrPendingNotFound) || (err == nil && p.State != "staged") {
		writeError(w, http.StatusNotFound, "not_found", "Pending install not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if p.ManifestSHA256 != in.ManifestSHA256 {
		writeError(w, http.StatusConflict, "install_review_mismatch", "This pending install is not the manifest you reviewed; preview it again")
		return
	}
	var reviewed appPreview
	if err := json.Unmarshal([]byte(p.Preview), &reviewed); err != nil {
		writeInternalError(w, err)
		return
	}

	req, ierr := s.buildProvisionRequest(r, workspaceID, owner.ID, p, &reviewed)
	if ierr != nil {
		var ae *appInstallError
		if errors.As(ierr, &ae) {
			ae.write(w)
			return
		}
		writeInternalError(w, ierr)
		return
	}

	res, err := s.store.ProvisionAppInstall(*req)
	if err != nil {
		var pc *store.ProvisionConflictError
		switch {
		case errors.As(err, &pc):
			details := map[string]interface{}{"field": pc.Field}
			if pc.Artifact != "" {
				details["artifact"] = pc.Artifact
			}
			if pc.Collection != "" {
				details["collection"] = pc.Collection
			}
			writeError2(w, http.StatusConflict, "install_conflict", "The install was not applied, and nothing was written: "+pc.Error()+". Preview it again, or discard it.", details)
		case errors.Is(err, store.ErrPendingNotStaged):
			writeError(w, http.StatusNotFound, "not_found", "Pending install not found")
		case errors.Is(err, store.ErrPendingManifestChanged):
			writeError(w, http.StatusConflict, "install_review_mismatch", "This pending install is not the manifest you reviewed; preview it again")
		default:
			writeInternalError(w, err)
		}
		return
	}

	out := appInstallConfirmResponse{InstallID: res.InstallID, InstallCode: res.InstallCode, ExpiresAt: res.ExpiresAt, Notice: appInstallCodeNotice}
	actor, source := actorFromRequest(r)
	actorName := actorNameFromRequest(r)
	for i, item := range res.Items {
		coll := reviewed.Artifacts[i].DestinationCollection
		s.logActivity(workspaceID, item.ID, "created", r)
		s.publishItemEventWithName(sseItemCreated, workspaceID, item.ID, item.Title, coll, actor, actorName, source, item.Seq)
		out.Items = append(out.Items, confirmItem{Key: req.Artifacts[i].Key, Ref: item.Ref, Status: itemStatus(item)})
	}
	s.logAuditEvent(models.ActionAppInstalled, r, auditMeta(map[string]string{
		"workspace_id": workspaceID, "install_id": res.InstallID, "origin": p.Origin, "manifest_sha256": p.ManifestSHA256,
	}))
	writeJSON(w, http.StatusCreated, out)
}

// buildProvisionRequest re-normalizes the pending record's STAGED bytes with
// the preview's own code and compares each artifact with what the owner
// reviewed. A difference means the workspace changed since the review, and
// the owner previews again; nothing is written (409 install_review_stale).
func (s *Server) buildProvisionRequest(r *http.Request, workspaceID, ownerID string, p *store.PendingInstall, reviewed *appPreview) (*store.ProvisionRequest, error) {
	blobs, err := s.store.PendingInstallBlobs(p.ID)
	if err != nil {
		return nil, err
	}
	mblob, ok := blobs[store.PendingManifestKey]
	if !ok {
		return nil, fmt.Errorf("apps: pending install %s has no staged manifest", p.ID)
	}
	m, err := appmanifest.Parse(mblob.Data)
	if err != nil {
		return nil, fmt.Errorf("apps: staged manifest no longer parses: %w", err)
	}
	raws := make([][]byte, len(m.CompanionPack.Artifacts))
	for i, a := range m.CompanionPack.Artifacts {
		b, ok := blobs[a.Key]
		if !ok {
			return nil, fmt.Errorf("apps: pending install %s is missing artifact %q", p.ID, a.Key)
		}
		raws[i] = b.Data
	}
	fresh, err := s.buildAppPreview(r, workspaceID, m, p.ManifestSHA256, raws)
	if err != nil {
		var ae *appInstallError
		if errors.As(err, &ae) {
			stale := installErr(http.StatusConflict, "install_review_stale", "The workspace changed since you reviewed this install, and nothing was written: %s. Preview it again.", ae.msg)
			stale.path = ae.path
			return nil, stale
		}
		return nil, err
	}
	if len(fresh.Artifacts) != len(reviewed.Artifacts) || len(fresh.Collections) != len(reviewed.Collections) {
		return nil, installErr(http.StatusConflict, "install_review_stale", "The install no longer matches what you reviewed; preview it again")
	}
	for i, c := range fresh.Collections {
		if c.Adopt != reviewed.Collections[i].Adopt {
			e := installErr(http.StatusConflict, "install_review_stale", "Collection %q changed since you reviewed this install (it would now be %s); preview it again", c.Key, adoptWord(c.Adopt))
			e.path = fmt.Sprintf("companion_pack.collections[%d].slug", i)
			return nil, e
		}
	}
	for i, a := range fresh.Artifacts {
		was := reviewed.Artifacts[i]
		if a.NormalizedSHA256 != was.NormalizedSHA256 {
			e := installErr(http.StatusConflict, "install_review_stale", "Artifact %q would now be stored differently from what you reviewed (%s); preview it again", a.Key, describeNormalizedDiff(was.Normalized, a.Normalized))
			e.path = fmt.Sprintf("companion_pack.artifacts[%d]", i)
			return nil, e
		}
	}

	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	digests := map[string]any{"manifest": p.ManifestSHA256}
	artDigests := map[string]map[string]string{}
	req := &store.ProvisionRequest{
		PendingID: p.ID, WorkspaceID: workspaceID, OwnerID: ownerID, Origin: m.Origin,
		ManifestSHA256: p.ManifestSHA256, ManifestVersion: m.Version, ManifestJSON: string(manifestJSON),
		AppTitle: m.Title, ServiceAccess: m.Scopes.Service.Access, DelegatedAccess: m.Scopes.Delegated.Access,
		RedirectURIs: m.RedirectURIs, SourcePack: m.Origin + "@" + m.Version,
	}
	for i, c := range m.CompanionPack.Collections {
		req.Collections = append(req.Collections, store.ProvisionCollection{
			Key: c.Key, Slug: c.Slug, Name: c.Name, Schema: string(c.Schema), Adopt: fresh.Collections[i].Adopt,
		})
	}
	for _, a := range fresh.Artifacts {
		req.Artifacts = append(req.Artifacts, store.ProvisionArtifact{
			Key: a.Key, CollectionID: a.collectionID, Title: a.Normalized.Title, Content: a.Normalized.Content,
			Fields: a.Normalized.Fields, UniqueKeys: a.uniqueKeys, RelationTargets: a.relationTargets,
			RawSHA256: a.RawSHA256, NormalizedSHA256: a.NormalizedSHA256,
		})
		artDigests[a.Key] = map[string]string{"raw": a.RawSHA256, "normalized": a.NormalizedSHA256}
	}
	digests["artifacts"] = artDigests
	b, err := json.Marshal(digests)
	if err != nil {
		return nil, err
	}
	req.DigestsJSON = string(b)
	return req, nil
}

func adoptWord(adopt bool) string {
	if adopt {
		return "adopted from an earlier install"
	}
	return "created new"
}

// describeNormalizedDiff names what changed between the reviewed and the
// fresh normalization: the title, the body, and each field key by name.
func describeNormalizedDiff(was, now normalizedItem) string {
	var parts []string
	if was.Title != now.Title {
		parts = append(parts, "title")
	}
	if was.Content != now.Content {
		parts = append(parts, "body")
	}
	keys := map[string]bool{}
	for k := range was.Fields {
		keys[k] = true
	}
	for k := range now.Fields {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		a, _ := json.Marshal(was.Fields[k])
		b, _ := json.Marshal(now.Fields[k])
		if string(a) != string(b) {
			// The values name the collision: e.g. invocation_slug "ship"
			// reviewed, "ship-2" now, because another item took "ship".
			changed = append(changed, fmt.Sprintf("field %s: reviewed %s, now %s", k, a, b))
		}
	}
	sort.Strings(changed)
	parts = append(parts, changed...)
	if len(parts) == 0 {
		return "its digest differs"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

func itemStatus(item *models.Item) string {
	var f map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &f); err != nil {
		return ""
	}
	st, _ := f["status"].(string)
	return st
}

func (s *Server) handleIssueAppInstallCode(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	code, exp, err := s.store.IssueInstallCode(workspaceID, installID)
	if errors.Is(err, store.ErrInstallNotActive) {
		writeError(w, http.StatusNotFound, "not_found", "Active install not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.logAuditEvent(models.ActionAppInstallCodeIssued, r, auditMeta(map[string]string{"workspace_id": workspaceID, "install_id": installID}))
	writeJSON(w, http.StatusCreated, map[string]any{"install_id": installID, "install_code": code, "expires_at": exp, "notice": appInstallCodeNotice})
}

// handleRedeemAppInstallCode is mounted outside every auth middleware: the
// code is the credential. Every refusal is the same 400 so the route says
// nothing about which codes or installs exist.
func (s *Server) handleRedeemAppInstallCode(w http.ResponseWriter, r *http.Request) {
	if !s.appsAvailable() {
		writeError(w, http.StatusNotFound, "not_found", "Apps are not enabled on this server")
		return
	}
	if s.rateLimiters != nil && !s.rateLimiters.AppRedeemAddr.allow(rateLimitAddr(clientIP(r))) {
		writeRateLimitResponse(w, s.rateLimiters.AppRedeemAddr.config)
		return
	}
	// decodeJSONWithLimit reads the WHOLE body under the cap (a bare
	// json.Decoder stops after the first value, so a body padded past the cap
	// after a valid object was never refused) and applies the NUL refusal.
	var in struct {
		Code string `json:"code"`
	}
	if err := decodeJSONWithLimit(r, &in, appRedeemMaxBody); err != nil || in.Code == "" {
		writeError(w, http.StatusBadRequest, "invalid_install_code", "Invalid install code")
		return
	}
	if s.rateLimiters != nil {
		if installID, ok := s.store.InstallForCode(in.Code); ok && !s.rateLimiters.AppRedeemInstall.allow(installID) {
			writeRateLimitResponse(w, s.rateLimiters.AppRedeemInstall.config)
			return
		}
	}
	red, err := s.store.RedeemInstallCode(in.Code)
	if errors.Is(err, store.ErrInstallCodeInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_install_code", "Invalid install code")
		return
	}
	if err != nil {
		slog.Error("apps: redeem failed", "error", err)
		writeInternalError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"install_id": red.InstallID, "client_id": red.ClientID, "client_secret": red.ClientSecret,
	})
}
