package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App upgrade (SPEC-6 U8b2, DOC-3371 §2 "Upgrade"; TASK-3397).
//
// POST /workspaces/{ws}/apps/{installID}/upgrade/preview
//   stage the app's current manifest and artifacts exactly as an install
//   does, normalize them, and diff them against the install (classes in
//   app_upgrade_diff.go). Answers the preview with an "upgrade" member.
// POST /workspaces/{ws}/apps/{installID}/upgrade/confirm {pending_id, manifest_sha256}
//   apply it in one transaction that re-derives the normalization AND the
//   diff under locks and refuses if either moved (store.UpgradeAppInstall).
//
// Every upgrade, removals-only included, needs this confirm (lead ruling 3).

const appUpgradeNotice = "Review what changes. Changed artifacts arrive as new drafts; nothing you have activated is overwritten. Removed companion collections are released to the workspace with their data."

func (s *Server) installUpgradeBase(installState *store.InstallUpgradeState) (*appmanifest.Manifest, map[string]storedArtifactDigest, error) {
	old, err := appmanifest.Parse([]byte(installState.ManifestJSON))
	if err != nil {
		return nil, nil, fmt.Errorf("apps: the installed manifest of %s no longer parses: %w", installState.ID, err)
	}
	var stored struct {
		Artifacts map[string]storedArtifactDigest `json:"artifacts"`
	}
	if installState.DigestsJSON != "" {
		if err := json.Unmarshal([]byte(installState.DigestsJSON), &stored); err != nil {
			return nil, nil, fmt.Errorf("apps: the install's digests do not parse: %w", err)
		}
	}
	if stored.Artifacts == nil {
		stored.Artifacts = map[string]storedArtifactDigest{}
	}
	return old, stored.Artifacts, nil
}

func (s *Server) handleAppUpgradePreview(w http.ResponseWriter, r *http.Request) {
	workspaceID, owner, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	inst, err := s.store.GetInstallUpgradeStateQ(s.store.Q(), workspaceID, installID)
	if err != nil {
		s.writeInstallLifecycleError(w, err)
		return
	}
	if inst.State != store.InstallActive && inst.State != store.InstallInactive {
		s.writeInstallLifecycleError(w, &store.InstallStateError{State: inst.State})
		return
	}
	old, digests, err := s.installUpgradeBase(inst)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if _, err := s.store.SweepExpiredPendingInstalls(); err != nil {
		slog.Warn("apps: sweeping expired pending installs failed", "error", err)
	}
	pending, err := s.store.ReservePendingUpgrade(workspaceID, owner.ID, inst.Origin, inst.ID)
	if errors.Is(err, store.ErrPendingLimit) {
		writeError(w, http.StatusTooManyRequests, "pending_install_limit", "Too many app installs are pending; finish or discard one and try again")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	preview, ierr := s.stageAppInstall(r, workspaceID, pending, inst.Origin, func(p *appPreview, m *appmanifest.Manifest) error {
		d, err := diffUpgrade(old, m, digests, p)
		if err != nil {
			return err
		}
		if err := s.upgradeStateChecks(s.store.Q(), workspaceID, inst.ID, old, d); err != nil {
			return err
		}
		entries := d.Entries
		if entries == nil {
			entries = []upgradeDiffEntry{}
		}
		p.Upgrade = &appUpgradePreview{
			InstallID: inst.ID, FromVersion: old.Version, FromManifestSHA256: inst.ManifestSHA256,
			Diff: entries, ReviewRequired: d.ReviewRequired, Notice: appUpgradeNotice,
		}
		return nil
	})
	if ierr != nil {
		if err := s.store.DeletePendingInstall(pending.ID); err != nil {
			slog.Warn("apps: deleting a failed pending upgrade", "pending", pending.ID, "error", err)
		}
		var ae *appInstallError
		if errors.As(ierr, &ae) {
			ae.write(w)
			return
		}
		writeInternalError(w, ierr)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

type appUpgradeConfirmResponse struct {
	InstallID string        `json:"install_id"`
	Version   string        `json:"version"`
	Items     []confirmItem `json:"items"`
}

func (s *Server) handleAppUpgradeConfirm(w http.ResponseWriter, r *http.Request) {
	workspaceID, owner, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	installID := chi.URLParam(r, "installID")
	var in struct {
		PendingID      string `json:"pending_id"`
		ManifestSHA256 string `json:"manifest_sha256"`
	}
	if err := decodeJSON(r, &in); err != nil || in.PendingID == "" || in.ManifestSHA256 == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Body must be {\"pending_id\": \"...\", \"manifest_sha256\": \"<the hash you reviewed>\"}")
		return
	}
	p, err := s.store.GetPendingInstall(in.PendingID, workspaceID, owner.ID)
	if errors.Is(err, store.ErrPendingNotFound) || (err == nil && (p.State != "staged" || p.UpgradeOf != installID)) {
		writeError(w, http.StatusNotFound, "not_found", "Pending upgrade not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if p.ManifestSHA256 != in.ManifestSHA256 {
		writeError(w, http.StatusConflict, "install_review_mismatch", "This pending upgrade is not the manifest you reviewed; preview it again")
		return
	}
	var reviewed appPreview
	if err := json.Unmarshal([]byte(p.Preview), &reviewed); err != nil || reviewed.Upgrade == nil {
		writeInternalError(w, fmt.Errorf("apps: pending upgrade %s has no upgrade preview: %v", p.ID, err))
		return
	}

	// The staged bytes are immutable (the pending row is locked in the
	// transaction, and only its deletion changes them), so they are read once
	// here; every workspace-state read happens inside derive, on the tx.
	blobs, err := s.store.PendingInstallBlobs(p.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	mblob, ok := blobs[store.PendingManifestKey]
	if !ok {
		writeInternalError(w, fmt.Errorf("apps: pending upgrade %s has no staged manifest", p.ID))
		return
	}
	m, err := appmanifest.Parse(mblob.Data)
	if err != nil {
		writeInternalError(w, fmt.Errorf("apps: staged manifest no longer parses: %w", err))
		return
	}
	raws := make([][]byte, len(m.CompanionPack.Artifacts))
	for i, a := range m.CompanionPack.Artifacts {
		b, ok := blobs[a.Key]
		if !ok {
			writeInternalError(w, fmt.Errorf("apps: pending upgrade %s is missing artifact %q", p.ID, a.Key))
			return
		}
		raws[i] = b.Data
	}

	var plan *store.UpgradePlan // the last derivation's, to label the created items
	derive := func(q store.Queryer, inst *store.InstallUpgradeState) (*store.UpgradePlan, error) {
		fresh, err := s.buildAppPreviewQ(q, r, workspaceID, m, p.ManifestSHA256, raws, installID)
		if err != nil {
			var ae *appInstallError
			if errors.As(err, &ae) {
				stale := installErr(http.StatusConflict, "install_review_stale", "The workspace changed since you reviewed this upgrade, and nothing was written: %s. Preview it again.", ae.msg)
				stale.path = ae.path
				return nil, stale
			}
			return nil, err
		}
		if err := compareWithReviewed(fresh, &reviewed); err != nil {
			return nil, err
		}
		old, digests, err := s.installUpgradeBase(inst)
		if err != nil {
			return nil, err
		}
		d, err := diffUpgrade(old, m, digests, fresh)
		if err != nil {
			return nil, err
		}
		if err := s.upgradeStateChecks(q, workspaceID, installID, old, d); err != nil {
			return nil, err
		}
		entries := d.Entries
		if entries == nil {
			entries = []upgradeDiffEntry{}
		}
		if !reflect.DeepEqual(entries, reviewed.Upgrade.Diff) {
			return nil, installErr(http.StatusConflict, "install_review_stale", "What this upgrade changes is no longer what you reviewed; preview it again")
		}
		pl, err := upgradePlanFrom(old, m, d, fresh, digests)
		plan = pl
		return pl, err
	}

	items, err := s.store.UpgradeAppInstall(store.UpgradeRequest{
		PendingID: p.ID, WorkspaceID: workspaceID, OwnerID: owner.ID, InstallID: installID,
		ManifestSHA256: p.ManifestSHA256, FromManifestSHA256: reviewed.Upgrade.FromManifestSHA256,
	}, derive)
	if err != nil {
		var ae *appInstallError
		var pc *store.ProvisionConflictError
		switch {
		case errors.As(err, &ae):
			ae.write(w)
		case errors.As(err, &pc):
			writeError2(w, http.StatusConflict, "install_conflict", "The upgrade was not applied, and nothing was written: "+pc.Error()+". Preview it again, or discard it.", map[string]interface{}{"field": pc.Field})
		case errors.Is(err, store.ErrInstallMoved):
			writeError(w, http.StatusConflict, "install_moved", "The install changed since you previewed this upgrade (another upgrade, a disable or an uninstall); nothing was written. Preview it again.")
		case errors.Is(err, store.ErrPendingNotStaged):
			writeError(w, http.StatusNotFound, "not_found", "Pending upgrade not found")
		case errors.Is(err, store.ErrPendingManifestChanged):
			writeError(w, http.StatusConflict, "install_review_mismatch", "This pending upgrade is not the manifest you reviewed; preview it again")
		case errors.Is(err, store.ErrNotWorkspaceOwner):
			writeError(w, http.StatusForbidden, "forbidden", "Only a workspace owner can upgrade apps")
		default:
			s.writeInstallLifecycleError(w, err)
		}
		return
	}

	out := appUpgradeConfirmResponse{InstallID: installID, Version: m.Version}
	actor, source := actorFromRequest(r)
	actorName := actorNameFromRequest(r)
	collOf := map[string]string{}
	for _, a := range reviewed.Artifacts {
		collOf[a.Key] = a.DestinationCollection
	}
	for i, item := range items {
		key := plan.Artifacts[i].Key // the store creates them in plan order
		s.logActivity(workspaceID, item.ID, "created", r)
		s.publishItemEventWithName(sseItemCreated, workspaceID, item.ID, item.Title, collOf[key], actor, actorName, source, item.Seq)
		out.Items = append(out.Items, confirmItem{Key: key, Ref: item.Ref, Status: itemStatus(item)})
	}
	s.logAuditEvent(models.ActionAppInstallLifecycle, r, auditMeta(map[string]string{
		"workspace_id": workspaceID, "install_id": installID, "action": "upgrade",
		"from_manifest_sha256": reviewed.Upgrade.FromManifestSHA256, "manifest_sha256": p.ManifestSHA256,
	}))
	writeJSON(w, http.StatusOK, out)
}

// upgradePlanFrom turns a re-derived diff into what the store writes.
func upgradePlanFrom(old, next *appmanifest.Manifest, d *upgradeDiff, fresh *appPreview, oldDigests map[string]storedArtifactDigest) (*store.UpgradePlan, error) {
	manifestJSON, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	// A changed or added artifact records the digests of the draft it lands
	// as; an unchanged one keeps the digests of the item it installed.
	artDigests := map[string]storedArtifactDigest{}
	for _, a := range fresh.Artifacts {
		if d.ChangedArtifacts[a.Key] {
			artDigests[a.Key] = storedArtifactDigest{Raw: a.RawSHA256, Normalized: a.NormalizedSHA256}
		} else if was, ok := oldDigests[a.Key]; ok {
			artDigests[a.Key] = was
		}
	}
	digests, err := json.Marshal(map[string]any{"manifest": fresh.ManifestSHA256, "artifacts": artDigests})
	if err != nil {
		return nil, err
	}
	plan := &store.UpgradePlan{
		ManifestSHA256: fresh.ManifestSHA256, ManifestVersion: next.Version, ManifestJSON: string(manifestJSON),
		DigestsJSON: string(digests), ServiceAccess: next.Scopes.Service.Access, DelegatedAccess: next.Scopes.Delegated.Access,
		SourcePack: next.Origin + "@" + next.Version, Released: d.Released, Renamed: map[string]string{},
		Webhook: appWebhookSpec(next), Actions: appActionSpecs(next),
		Restrictive: len(d.Released) > 0 ||
			accessRank(next.Scopes.Service.Access) < accessRank(old.Scopes.Service.Access) ||
			accessRank(next.Scopes.Delegated.Access) < accessRank(old.Scopes.Delegated.Access),
	}
	if !sameStringSet(old.RedirectURIs, next.RedirectURIs) {
		plan.RedirectURIs = append([]string{}, next.RedirectURIs...)
	}
	oldColl := map[string]appmanifest.Collection{}
	for _, c := range old.CompanionPack.Collections {
		oldColl[c.Key] = c
	}
	for _, c := range next.CompanionPack.Collections {
		o, existed := oldColl[c.Key]
		if !existed {
			plan.NewCollections = append(plan.NewCollections, store.ProvisionCollection{Key: c.Key, Slug: c.Slug, Name: c.Name, Schema: string(c.Schema)})
			continue
		}
		if o.Name != c.Name {
			plan.Renamed[c.Slug] = c.Name
		}
	}
	for _, add := range d.SchemaAdds {
		plan.SchemaAdds = append(plan.SchemaAdds, store.UpgradeSchemaAdd{CollectionSlug: add.CollectionSlug, Field: add.Field})
	}
	for _, a := range fresh.Artifacts {
		if !d.ChangedArtifacts[a.Key] {
			continue
		}
		plan.Artifacts = append(plan.Artifacts, store.ProvisionArtifact{
			Key: a.Key, CollectionID: a.collectionID, Title: a.Normalized.Title, Content: a.Normalized.Content,
			Fields: a.Normalized.Fields, RawSHA256: a.RawSHA256, NormalizedSHA256: a.NormalizedSHA256,
		})
	}
	return plan, nil
}

// upgradeStateChecks runs, on q, the checks the diff cannot make alone
// because they read the workspace: every installed companion still resolves,
// and every additive field is safe for the collection's existing items
// (codex r1 on U8b2). Preview runs it on the pool (advisory); confirm runs it
// inside the transaction (the guarantee).
func (s *Server) upgradeStateChecks(q store.Queryer, workspaceID, installID string, old *appmanifest.Manifest, d *upgradeDiff) error {
	var slugs []string
	for _, c := range old.CompanionPack.Collections {
		slugs = append(slugs, c.Slug)
	}
	if err := s.store.CheckCompanionsResolveQ(q, workspaceID, installID, slugs); err != nil {
		return upgradeConflictErr(err)
	}
	bySlug := map[string][]models.FieldDef{}
	var order []string
	for _, add := range d.SchemaAdds {
		if _, seen := bySlug[add.CollectionSlug]; !seen {
			order = append(order, add.CollectionSlug)
		}
		bySlug[add.CollectionSlug] = append(bySlug[add.CollectionSlug], add.Field)
	}
	for _, slug := range order {
		if err := s.store.CheckAdditiveFieldsQ(q, workspaceID, installID, slug, bySlug[slug]); err != nil {
			return upgradeConflictErr(err)
		}
	}
	return nil
}

func upgradeConflictErr(err error) error {
	var pc *store.ProvisionConflictError
	if errors.As(err, &pc) {
		e := installErr(http.StatusConflict, "upgrade_conflict", "This upgrade cannot apply to the workspace as it stands: %s", pc.Error())
		e.path = pc.Field
		return e
	}
	return err
}
