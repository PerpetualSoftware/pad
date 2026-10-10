package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appfetch"
	"github.com/PerpetualSoftware/pad/internal/appmanifest"
	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// App install staging and preview (SPEC-6 U8a, DOC-3371 §2 steps 1-5, §9;
// TASK-3397). Provisioning, the install code and redeem are U8b.
//
// POST /workspaces/{ws}/apps/install/preview {base_url}
//   reserve (owner + instance caps) -> fetch the manifest and every artifact
//   exactly once under the fetch policy -> stage the raw bytes -> validate
//   apps/1 -> normalize each artifact with the importer's own preprocess ->
//   conflict check -> a preview the owner reviews before consenting.
// GET    /workspaces/{ws}/apps/install/pending/{pendingID}  the staged preview
// DELETE /workspaces/{ws}/apps/install/pending/{pendingID}  discard it
//
// Owner-only, and only while appsAvailable(). Nothing is provisioned here.

// settingAppsPrivateOrigins is the admin's private-destination list (§9),
// JSON-encoded []appfetch.PrivateOrigin. Self-host only.
const settingAppsPrivateOrigins = "apps_private_origins"

// appFetchDeadline bounds one install's whole fetch (§2 caps).
const appFetchDeadline = 30 * time.Second

// appArtifactMaxBytes is the per-artifact cap: the importer's own default
// (§2: defaultImportArtifactMaxBytes).
const appArtifactMaxBytes = defaultImportArtifactMaxBytes

// appNotReviewedNotice is shown with every app on every surface (Dave Q3).
const appNotReviewedNotice = "Not reviewed by Pad. You are installing code published at this origin."

// appDeferredNotice explains declared webhook events and item actions that
// this server does not provision yet (A5/A6). They are stored with the
// install and activated when supported, without a reinstall.
const appDeferredNotice = "Webhook events and item actions will activate when this server supports them; they are recorded with the install now."

// appPreview is what the owner reviews before consenting (§2 step 4).
type appPreview struct {
	PendingID       string                   `json:"pending_id"`
	ExpiresAt       time.Time                `json:"expires_at"`
	Origin          string                   `json:"origin"`
	ManifestURL     string                   `json:"manifest_url"`
	ManifestSHA256  string                   `json:"manifest_sha256"`
	AppID           string                   `json:"app_id"`
	Version         string                   `json:"version"`
	Title           string                   `json:"title"`
	Description     string                   `json:"description,omitempty"`
	Publisher       string                   `json:"publisher"`
	Homepage        string                   `json:"homepage,omitempty"`
	ReviewedByPad   bool                     `json:"reviewed_by_pad"`
	Notice          string                   `json:"notice"`
	ServiceAccess   string                   `json:"service_access"`
	DelegatedAccess string                   `json:"delegated_access"`
	ReadsSystem     string                   `json:"reads_system_collections"`
	Collections     []appPreviewCollection   `json:"collections"`
	Events          []appmanifest.Event      `json:"events"`
	WebhookURL      string                   `json:"webhook_url,omitempty"`
	ItemActions     []appmanifest.ItemAction `json:"item_actions"`
	DeferredNotice  string                   `json:"deferred_notice,omitempty"`
	Artifacts       []appPreviewArtifact     `json:"artifacts"`
	ConfigSchema    json.RawMessage          `json:"config_schema,omitempty"`
	RedirectURIs    []string                 `json:"redirect_uris"`
	Docs            string                   `json:"docs,omitempty"`
	// Upgrade is set on an upgrade's preview (U8b2): what it would change.
	Upgrade *appUpgradePreview `json:"upgrade,omitempty"`
}

// withLists returns p with every list a list, never null (BUG-3417): the
// manifest's optional lists are nil when it omits them, and a nil slice
// serializes as null where the contract (and the TS type) say an array. Every
// door that writes a preview calls it.
func (p *appPreview) withLists() *appPreview {
	if p.Collections == nil {
		p.Collections = []appPreviewCollection{}
	}
	if p.Events == nil {
		p.Events = []appmanifest.Event{}
	}
	if p.ItemActions == nil {
		p.ItemActions = []appmanifest.ItemAction{}
	}
	if p.Artifacts == nil {
		p.Artifacts = []appPreviewArtifact{}
	}
	if p.RedirectURIs == nil {
		p.RedirectURIs = []string{}
	}
	return p
}

// appUpgradePreview is the upgrade half of a preview (U8b2).
type appUpgradePreview struct {
	InstallID          string             `json:"install_id"`
	FromVersion        string             `json:"from_version"`
	FromManifestSHA256 string             `json:"from_manifest_sha256"`
	Diff               []upgradeDiffEntry `json:"diff"`
	ReviewRequired     bool               `json:"review_required"`
	Notice             string             `json:"notice"`
}

type appPreviewCollection struct {
	Key    string          `json:"key"`
	Slug   string          `json:"slug"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	// Adopt: a collection with this slug exists and was created by an install
	// of this same origin; it is adopted rather than created.
	Adopt bool `json:"adopt"`
	// Existing: on an upgrade, this install's own companion (U8b2).
	Existing bool `json:"existing,omitempty"`
}

type appPreviewArtifact struct {
	Key                   string         `json:"key"`
	URL                   string         `json:"url"`
	Kind                  string         `json:"kind"`
	DestinationCollection string         `json:"destination_collection"`
	RawSHA256             string         `json:"raw_sha256"`
	Raw                   string         `json:"raw"`
	Normalized            normalizedItem `json:"normalized"`
	NormalizedSHA256      string         `json:"normalized_sha256"`
	// Changes are the importer's warnings: every field it dropped or changed.
	Changes []string `json:"changes"`

	// For provisioning (U8b), never serialized: the destination collection.
	collectionID string
}

// normalizedItem is the item an artifact will be stored as. Its canonical
// JSON (encoding/json sorts map keys) is what normalized_sha256 hashes.
type normalizedItem struct {
	Title   string         `json:"title"`
	Content string         `json:"content"`
	Fields  map[string]any `json:"fields"`
}

func (n normalizedItem) digest() (string, error) {
	b, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	return appmanifest.SHA256Hex(sha256.Sum256(b)), nil
}

// appInstallError is a refusal the preview reports to the owner.
type appInstallError struct {
	status int
	code   string
	msg    string
	path   string
}

func (e *appInstallError) Error() string { return e.msg }

func installErr(status int, code, format string, args ...any) *appInstallError {
	return &appInstallError{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func (e *appInstallError) write(w http.ResponseWriter) {
	details := map[string]interface{}{}
	if e.path != "" {
		details["path"] = e.path
	}
	writeError2(w, e.status, e.code, e.msg, details)
}

// requireAppsOwner gates every install route: apps available here, and the
// caller an owner of the workspace.
func (s *Server) requireAppsOwner(w http.ResponseWriter, r *http.Request) (workspaceID string, owner *models.User, ok bool) {
	if !s.appsAvailable() {
		writeError(w, http.StatusNotFound, "not_found", "Apps are not enabled on this server")
		return "", nil, false
	}
	if !requireMinRole(w, r, "owner") {
		return "", nil, false
	}
	workspaceID, ok = s.getWorkspaceID(w, r)
	if !ok {
		return "", nil, false
	}
	owner = currentUser(r)
	if owner == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to install apps")
		return "", nil, false
	}
	return workspaceID, owner, true
}

func (s *Server) handleAppInstallPreview(w http.ResponseWriter, r *http.Request) {
	workspaceID, owner, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	var in struct {
		BaseURL string `json:"base_url"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Body must be {\"base_url\": \"https://...\"}")
		return
	}
	origin, err := appmanifest.NormalizeOrigin(in.BaseURL)
	if err != nil {
		writeError2(w, http.StatusBadRequest, "invalid_base_url", "base_url "+err.Error(), map[string]interface{}{"path": "base_url"})
		return
	}
	if _, err := s.store.SweepExpiredPendingInstalls(); err != nil {
		slog.Warn("apps: sweeping expired pending installs failed", "error", err)
	}

	pending, err := s.store.ReservePendingInstall(workspaceID, owner.ID, origin)
	if errors.Is(err, store.ErrPendingLimit) {
		writeError(w, http.StatusTooManyRequests, "pending_install_limit", "Too many app installs are pending; finish or discard one and try again")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	preview, ierr := s.stageAppInstall(r, workspaceID, pending, origin, nil)
	if ierr != nil {
		if err := s.store.DeletePendingInstall(pending.ID); err != nil {
			slog.Warn("apps: deleting a failed pending install", "pending", pending.ID, "error", err)
		}
		var ae *appInstallError
		if errors.As(ierr, &ae) {
			ae.write(w)
			return
		}
		writeInternalError(w, ierr)
		return
	}
	writeJSON(w, http.StatusOK, preview.withLists())
}

// stageAppInstall fetches, stages, validates and previews under a reservation
// already taken. Any error leaves the caller to delete the reservation.
//
// extend, when set (an upgrade, U8b2), adds to the preview before it is
// stored; a refusal from it is the stage's refusal.
func (s *Server) stageAppInstall(r *http.Request, workspaceID string, pending *store.PendingInstall, origin string, extend func(*appPreview, *appmanifest.Manifest) error) (*appPreview, error) {
	ctx, cancel := context.WithTimeout(r.Context(), appFetchDeadline)
	defer cancel()
	fetcher, err := appfetch.New(s.appsPrivateOrigins(), appFetchDeadline, s.appFetchTLS)
	if err != nil {
		return nil, fmt.Errorf("apps: private origin list: %w", err)
	}

	manifestURL := appmanifest.ManifestURL(origin)
	raw, err := fetcher.Get(ctx, manifestURL, appmanifest.MaxManifestBytes)
	if err != nil {
		return nil, installErr(http.StatusBadGateway, "manifest_fetch_failed", "Could not fetch %s: %v", manifestURL, err)
	}
	manifestSHA := appmanifest.SHA256Hex(sha256.Sum256(raw))
	if err := s.store.StagePendingBlob(pending.ID, store.PendingBlob{Key: store.PendingManifestKey, URL: manifestURL, SHA256: manifestSHA, Data: raw}); err != nil {
		return nil, s.stageErr(err)
	}
	m, err := appmanifest.Parse(raw)
	if err != nil {
		var me *appmanifest.Error
		if errors.As(err, &me) {
			e := installErr(http.StatusUnprocessableEntity, "invalid_manifest", "%s", me.Error())
			e.path = me.Path
			return nil, e
		}
		return nil, err
	}
	if m.Origin != origin {
		e := installErr(http.StatusUnprocessableEntity, "invalid_manifest", "the manifest's base_url (%s) is not the origin it was fetched from (%s)", m.Origin, origin)
		e.path = "base_url"
		return nil, e
	}

	// Fetch every artifact exactly once, checking the declared sha256.
	rawArtifacts := make([][]byte, len(m.CompanionPack.Artifacts))
	for i, a := range m.CompanionPack.Artifacts {
		body, err := fetcher.Get(ctx, a.URL, appArtifactMaxBytes)
		if err != nil {
			e := installErr(http.StatusBadGateway, "artifact_fetch_failed", "Could not fetch artifact %q: %v", a.Key, err)
			e.path = fmt.Sprintf("companion_pack.artifacts[%d]", i)
			return nil, e
		}
		if got := appmanifest.SHA256Hex(sha256.Sum256(body)); got != a.SHA256 {
			e := installErr(http.StatusUnprocessableEntity, "artifact_hash_mismatch", "Artifact %q does not match its declared sha256", a.Key)
			e.path = fmt.Sprintf("companion_pack.artifacts[%d].sha256", i)
			return nil, e
		}
		if err := s.store.StagePendingBlob(pending.ID, store.PendingBlob{Key: a.Key, URL: a.URL, SHA256: a.SHA256, Data: body}); err != nil {
			return nil, s.stageErr(err)
		}
		rawArtifacts[i] = body
	}
	// No further bytes can arrive: everything is staged.

	preview, err := s.buildAppPreviewQ(s.store.Q(), r, workspaceID, m, manifestSHA, rawArtifacts, pending.UpgradeOf)
	if err != nil {
		return nil, err
	}
	preview.PendingID = pending.ID
	if extend != nil {
		if err := extend(preview, m); err != nil {
			return nil, err
		}
	}
	// The stored preview omits each artifact's raw text: the staged blobs
	// already hold those bytes, and GET fills them back in from there.
	stored := *preview
	stored.Artifacts = make([]appPreviewArtifact, len(preview.Artifacts))
	for i, a := range preview.Artifacts {
		a.Raw = ""
		stored.Artifacts[i] = a
	}
	previewJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	if err := s.store.FinishPendingInstall(pending.ID, manifestSHA, string(previewJSON)); err != nil {
		return nil, s.stageErr(err)
	}
	got, err := s.store.GetPendingInstall(pending.ID, workspaceID, pending.OwnerID)
	if err != nil {
		return nil, err
	}
	preview.ExpiresAt = got.ExpiresAt
	return preview, nil
}

func (s *Server) stageErr(err error) error {
	switch {
	case errors.Is(err, store.ErrPendingOverCap):
		return installErr(http.StatusRequestEntityTooLarge, "pack_too_large", "The app's manifest and artifacts exceed the %d MiB install cap", store.PendingInstallBytes>>20)
	case errors.Is(err, store.ErrPendingNotFound):
		return installErr(http.StatusGatewayTimeout, "install_fetch_expired", "The install took too long to fetch; try again")
	}
	return err
}

// buildAppPreviewQ validates the pack against this workspace and normalizes
// every artifact exactly as the importer would store it, with every read on
// q: the pool for a preview, the transaction for provisioning (U8b), which
// runs it under its locks and compares the result
// with the reviewed preview: the digest comparison IS the re-check, so no
// fact the normalization depends on has to be listed separately (lead
// ruling, day 86). It must stay read-only: the write-capture test asserts a
// pass on the provisioning transaction writes nothing.
//
// upgradeOf is "" for an install; for an upgrade (U8b2) it is the install
// being upgraded, whose own companion collections are existing, not
// conflicts.
func (s *Server) buildAppPreviewQ(q store.Queryer, r *http.Request, workspaceID string, m *appmanifest.Manifest, manifestSHA string, rawArtifacts [][]byte, upgradeOf string) (*appPreview, error) {
	p := &appPreview{
		Origin: m.Origin, ManifestURL: appmanifest.ManifestURL(m.Origin), ManifestSHA256: manifestSHA,
		AppID: m.ID, Version: m.Version, Title: m.Title, Description: m.Description, Publisher: m.Publisher, Homepage: m.Homepage,
		ReviewedByPad: false, Notice: appNotReviewedNotice,
		ServiceAccess: m.Scopes.Service.Access, DelegatedAccess: m.Scopes.Delegated.Access,
		ReadsSystem: "The app can read this workspace's Conventions and Playbooks.",
		Events:      m.Events, WebhookURL: m.WebhookURL, ItemActions: m.ItemActions,
		ConfigSchema: m.ConfigSchema, RedirectURIs: m.RedirectURIs, Docs: m.Docs,
	}
	if len(m.Events) > 0 || len(m.ItemActions) > 0 {
		p.DeferredNotice = appDeferredNotice
	}

	// Conflict check (§2 step 5, L6).
	slugs := make([]string, len(m.CompanionPack.Collections))
	for i, c := range m.CompanionPack.Collections {
		slugs[i] = c.Slug
	}
	owners, err := s.store.CollectionSlugOwnersQ(q, workspaceID, slugs)
	if err != nil {
		return nil, err
	}
	for i, c := range m.CompanionPack.Collections {
		pc := appPreviewCollection{Key: c.Key, Slug: c.Slug, Name: c.Name, Schema: c.Schema}
		if store.IsReservedCollectionSlug(c.Slug) {
			e := installErr(http.StatusUnprocessableEntity, "invalid_manifest", "collection %q: the slug %q is reserved by Pad", c.Key, c.Slug)
			e.path = fmt.Sprintf("companion_pack.collections[%d].slug", i)
			return nil, e
		}
		if holder, exists := owners[c.Slug]; exists && upgradeOf != "" && holder.InstallID == upgradeOf {
			pc.Existing = true
		} else if exists {
			if holder.Origin != m.Origin {
				e := installErr(http.StatusConflict, "collection_conflict", "This workspace already has a collection %q that this app did not create", c.Slug)
				e.path = fmt.Sprintf("companion_pack.collections[%d].slug", i)
				return nil, e
			}
			// Adopted only from an UNINSTALLED install (lead ruling, day
			// 86): a disabled install can be re-enabled, and taking its
			// companions would break it then.
			if holder.InstallState != "uninstalled" {
				e := installErr(http.StatusConflict, "app_already_installed", "This app is already installed in this workspace (state %s): re-enable or uninstall it first", holder.InstallState)
				e.path = fmt.Sprintf("companion_pack.collections[%d].slug", i)
				return nil, e
			}
			pc.Adopt = true
		}
		if err := validateNoReservedFieldKeys(c.Parsed, nil); err != nil {
			e := installErr(http.StatusUnprocessableEntity, "invalid_manifest", "collection %q: %v", c.Key, err)
			e.path = fmt.Sprintf("companion_pack.collections[%d].schema", i)
			return nil, e
		}
		// The rule collection writes follow (TASK-3539), checked on the NEW
		// manifest here rather than in appmanifest.Parse, which also re-reads
		// an installed app's stored manifest.
		if err := models.ValidateSchemaKeysAndOptions(c.Parsed); err != nil {
			e := installErr(http.StatusUnprocessableEntity, "invalid_manifest", "collection %q: %v", c.Key, err)
			e.path = fmt.Sprintf("companion_pack.collections[%d].schema", i)
			return nil, e
		}
		p.Collections = append(p.Collections, pc)
	}

	// Unique values claimed by earlier artifacts of this same pack, per
	// destination collection and field: the import creates them in order, so
	// a later artifact de-collides its invocation_slug against them, and any
	// other unique-scoped value it repeats is the conflict the import's unique
	// check would refuse (codex rounds 2-4). Provisioning inserts in the same
	// order on one transaction and so reaches the same outcome.
	claimed := map[string]map[string]map[string]bool{}
	for i, a := range m.CompanionPack.Artifacts {
		pa, err := s.previewArtifact(q, r, workspaceID, a, rawArtifacts[i], claimed)
		if err != nil {
			var ae *appInstallError
			if errors.As(err, &ae) && ae.path == "" {
				ae.path = fmt.Sprintf("companion_pack.artifacts[%d]", i)
			}
			return nil, err
		}
		p.Artifacts = append(p.Artifacts, *pa)
	}
	return p, nil
}

// previewArtifact runs the importer's own guards, decode and preprocess over
// the STAGED bytes (DOC-3371 §2 step 3).
func (s *Server) previewArtifact(q store.Queryer, r *http.Request, workspaceID string, a appmanifest.Artifact, raw []byte, claimed map[string]map[string]map[string]bool) (*appPreviewArtifact, error) {
	art, err := decodeArtifactBytes(raw)
	if err != nil {
		return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q is not a valid Pad artifact: %v", a.Key, err)
	}
	if msg := models.ValidateItemTitle(models.NormalizeItemTitle(art.Title)); msg != "" {
		return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q: %s", a.Key, msg)
	}
	collID, err := s.collectionIDForKindQ(q, workspaceID, art.Kind, nil)
	if err != nil {
		return nil, err
	}
	if collID == "" {
		return nil, installErr(http.StatusUnprocessableEntity, "artifact_destination_missing", "This workspace has no collection that accepts %q artifacts (artifact %q)", art.Kind, a.Key)
	}
	coll, err := s.store.GetCollectionQ(q, collID)
	if err != nil {
		return nil, err
	}
	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(coll.Schema), &schema); err != nil {
		return nil, fmt.Errorf("apps: destination schema: %w", err)
	}
	if claimed[coll.ID] == nil {
		claimed[coll.ID] = map[string]map[string]bool{}
	}
	packClaims := claimed[coll.ID]
	norm, err := normalizeArtifact(art, coll, schema, func(slug string) (bool, error) {
		if packClaims["invocation_slug"]["s:"+slug] {
			return true, nil
		}
		return s.invocationSlugTakenQ(q, workspaceID, coll.ID, slug)
	})
	if err != nil {
		return nil, err
	}

	// The create's own field pipeline (coercion, defaults, validation,
	// relation resolution in the import's carry posture, the unique check),
	// so the digest covers exactly what the import stores (codex round 1).
	fields, dropped, unresolved, undeclared, cerr := s.prepareCreateFieldsQ(q, r, workspaceID, coll, schema, norm.Fields, relationsCarry)
	if cerr != nil {
		return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q cannot be stored in %q: %s", a.Key, coll.Slug, cerr.message)
	}
	for _, k := range undeclared {
		norm.Warnings = append(norm.Warnings, fmt.Sprintf("field %q is not declared by the destination collection's schema; stored as-is", k))
	}
	for _, k := range unresolved {
		norm.Warnings = append(norm.Warnings, fmt.Sprintf("field %q was imported as-is: it does not name an item in this workspace", k))
	}
	for _, k := range dropped {
		norm.Warnings = append(norm.Warnings, fmt.Sprintf("field %q was discarded: the destination collection's schema declares a default for it that is not a valid reference", k))
	}
	// Claims are recorded from the FINAL fields, after defaults, which is
	// what the import stores (codex round 3), for invocation_slug and every
	// unique-scoped field (codex round 4).
	// Which fields: invocation_slug always (a database unique index enforces
	// it whatever the schema says), plus exactly the fields the import's
	// checkUniqueFields enforces, through the same predicate (codex round 6).
	// Deduplicated: a schema may declare a key twice, and one artifact must
	// never conflict with itself (codex round 5).
	uniqueKeys := []string{"invocation_slug"}
	seenKey := map[string]bool{"invocation_slug": true}
	for _, def := range schema.Fields {
		if uniqueEnforced(def) && !seenKey[def.Key] {
			seenKey[def.Key] = true
			uniqueKeys = append(uniqueKeys, def.Key)
		}
	}
	// invocation_slug is also covered by a database unique index per
	// collection (056 / pg 033), whatever the schema says, so its FINAL value
	// (a default included) is checked against stored items too, through the
	// index's own expression (codex round 8), and must be
	// text: SQLite keys the index on the JSON-typed value and Postgres on its
	// text, so a non-text slug is refused rather than guessed at (codex round
	// 7). Provisioning re-checks every constraint inside one transaction, so
	// a divergence left here costs a failed install, never a partial one.
	if raw, ok := fields["invocation_slug"]; ok && raw != nil {
		slug, isText := raw.(string)
		if !isText {
			return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q would store a non-text invocation_slug", a.Key)
		}
		if slug != "" {
			taken, err := s.store.InvocationSlugIndexTakenQ(q, coll.ID, slug)
			if err != nil {
				return nil, err
			}
			if taken {
				return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q would store invocation_slug %q, which an item in %q already holds", a.Key, slug, coll.Slug)
			}
		}
	}
	for _, key := range uniqueKeys {
		// INVARIANT (lead ruling): the preview may refuse what the import
		// would accept, never the reverse; U8b's provisioning re-checks every
		// constraint inside one transaction, so that re-check is the
		// guarantee and this preview is advisory.
		//
		// Each value claims every form the import's lookup (JSONFieldEquals)
		// could match it by: text, boolean, and number as its float64 (equal
		// doubles is a superset of SQLite's double comparison and Postgres's
		// exact numeric equality). Two values collide when they share any
		// form (codex rounds 4-12).
		forms := claimForms(fields[key])
		for _, f := range forms {
			if packClaims[key][f] {
				return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q would store a %s value that an earlier artifact of this pack already holds", a.Key, key)
			}
		}
		if packClaims[key] == nil {
			packClaims[key] = map[string]bool{}
		}
		for _, f := range forms {
			packClaims[key][f] = true
		}
	}
	// App-authored content: the owner only REVIEWED it, so it may not carry
	// a pad-attachment: token or a [[workspace::REF]] link, and the human
	// path's attachment stamp and cross-workspace resolution never run on it
	// (lead ruling, U8b Q3). Ordinary [[Title]] links stay: they resolve
	// workspace-wide as on any owner write, and the preview shows them.
	if err := refuseAppAuthoredReferences(norm.Content, fields); err != nil {
		return nil, installErr(http.StatusUnprocessableEntity, "invalid_artifact", "Artifact %q: %v", a.Key, err)
	}
	item := normalizedItem{Title: norm.Title, Content: norm.Content, Fields: fields}
	digest, err := item.digest()
	if err != nil {
		return nil, err
	}
	changes := append([]string{}, norm.Warnings...)
	sort.Strings(changes)
	return &appPreviewArtifact{
		Key: a.Key, URL: a.URL, Kind: string(art.Kind), DestinationCollection: coll.Slug,
		RawSHA256: a.SHA256, Raw: string(raw), Normalized: item, NormalizedSHA256: digest, Changes: changes,
		collectionID: coll.ID,
	}, nil
}

func (s *Server) handleGetAppInstallPending(w http.ResponseWriter, r *http.Request) {
	workspaceID, owner, ok := s.requireAppsOwner(w, r)
	if !ok {
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
	var preview appPreview
	if err := json.Unmarshal([]byte(p.Preview), &preview); err != nil {
		writeInternalError(w, err)
		return
	}
	blobs, err := s.store.PendingInstallBlobs(p.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for i := range preview.Artifacts {
		preview.Artifacts[i].Raw = string(blobs[preview.Artifacts[i].Key].Data)
	}
	preview.ExpiresAt = p.ExpiresAt
	writeJSON(w, http.StatusOK, preview.withLists())
}

func (s *Server) handleDeleteAppInstallPending(w http.ResponseWriter, r *http.Request) {
	workspaceID, owner, ok := s.requireAppsOwner(w, r)
	if !ok {
		return
	}
	p, err := s.store.GetPendingInstall(chi.URLParam(r, "pendingID"), workspaceID, owner.ID)
	if errors.Is(err, store.ErrPendingNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Pending install not found")
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.store.DeletePendingInstall(p.ID); err != nil {
		writeInternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appsPrivateOrigins is the admin list, or nil on Cloud (no private
// destination is ever reachable there) or when unset or unreadable.
func (s *Server) appsPrivateOrigins() []appfetch.PrivateOrigin {
	if s.cloudMode {
		return nil
	}
	v, err := s.store.GetPlatformSetting(settingAppsPrivateOrigins)
	if err != nil || v == "" {
		return nil
	}
	var list []appfetch.PrivateOrigin
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		slog.Warn("apps: the private-origin list is not valid JSON; ignoring it", "error", err)
		return nil
	}
	return list
}

// SetAppFetchTLS overrides the TLS config app fetches use. Tests only.
func (s *Server) SetAppFetchTLS(c *tls.Config) { s.appFetchTLS = c }

// --- Admin: GET/PUT /api/v1/admin/apps ----------------------------------

type appsSettingsResponse struct {
	Enabled        bool                     `json:"enabled"`
	Available      bool                     `json:"available"`
	HTTPSIssuer    bool                     `json:"https_issuer"`
	Cloud          bool                     `json:"cloud"`
	PrivateOrigins []appfetch.PrivateOrigin `json:"private_origins"`
}

// withAllowedLists returns origins as a list, every entry's Allowed a list
// too (BUG-3417): a webhook-only entry may omit it, and a value stored
// before this answered null.
func withAllowedLists(origins []appfetch.PrivateOrigin) []appfetch.PrivateOrigin {
	out := make([]appfetch.PrivateOrigin, len(origins))
	for i, o := range origins {
		if o.Allowed == nil {
			o.Allowed = []string{}
		}
		out[i] = o
	}
	return out
}

func (s *Server) appsSettings() appsSettingsResponse {
	v, _ := s.store.GetPlatformSetting(settingAppsEnabled)
	list := withAllowedLists(s.appsPrivateOrigins())
	return appsSettingsResponse{
		Enabled: s.cloudMode || v == "true", Available: s.appsAvailable(),
		HTTPSIssuer: s.oauthServer != nil && (s.cloudMode || s.mcpEndpoints.HTTPS()),
		Cloud:       s.cloudMode, PrivateOrigins: list,
	}
}

// handleGetAppsSettings answers on Cloud too, as GET /admin/mcp does, with
// cloud: true: reading the state is harmless, and only the PUT is
// operator-managed there (managed_by_operator).
func (s *Server) handleGetAppsSettings(w http.ResponseWriter, r *http.Request) {
	if user := currentUser(r); user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}
	writeJSON(w, http.StatusOK, s.appsSettings())
}

func (s *Server) handleUpdateAppsSettings(w http.ResponseWriter, r *http.Request) {
	if user := currentUser(r); user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}
	if s.cloudMode {
		writeError(w, http.StatusForbidden, "managed_by_operator", "Apps on this instance are managed by the operator.")
		return
	}
	var in struct {
		Enabled        *bool                     `json:"enabled"`
		PrivateOrigins *[]appfetch.PrivateOrigin `json:"private_origins"`
	}
	if err := decodeJSON(r, &in); err != nil || (in.Enabled == nil && in.PrivateOrigins == nil) {
		writeError(w, http.StatusBadRequest, "bad_request", "Body must set enabled and/or private_origins")
		return
	}
	var changed []string
	if in.PrivateOrigins != nil {
		list := withAllowedLists(*in.PrivateOrigins)
		// Validate by building a fetcher from it: the same parser the fetch
		// path uses, so a list that saves is a list that works.
		if _, err := appfetch.New(list, time.Second, nil); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_private_origins", err.Error())
			return
		}
		b, err := json.Marshal(list)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if err := s.store.SetPlatformSetting(settingAppsPrivateOrigins, string(b)); err != nil {
			writeInternalError(w, err)
			return
		}
		changed = append(changed, settingAppsPrivateOrigins)
	}
	if in.Enabled != nil {
		value := "false"
		if *in.Enabled {
			value = "true"
		}
		if err := s.store.SetPlatformSetting(settingAppsEnabled, value); err != nil {
			writeInternalError(w, err)
			return
		}
		changed = append(changed, settingAppsEnabled)
	}
	s.logAuditEvent(models.ActionSettingsChanged, r, settingsChangedMeta(changed))
	writeJSON(w, http.StatusOK, s.appsSettings())
}

// claimForms are the forms a field value can be matched by in the import's
// uniqueness lookup (JSONFieldEquals): a string as text, and also as a
// boolean when it reads true/false and as a number when it is a JSON number;
// a boolean as a boolean; a number as its canonical number. Absent, null and
// empty values claim nothing, as they occupy no unique slot. Composite values
// claim their JSON text.
func claimForms(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		forms := []string{"s:" + t}
		if t == "true" || t == "false" {
			forms = append(forms, "b:"+t)
		}
		for _, n := range store.NumericClaimForms(t) {
			forms = append(forms, "n:"+n)
		}
		return forms
	case bool:
		return []string{fmt.Sprintf("b:%v", t)}
	case float64:
		// Canonicalized from its JSON serialization, which is the text the
		// stored value has and the import compares against, through the same
		// numericArg path a later string takes (codex round 11: formatting
		// the float directly diverged near 2^63 and past 2^53).
		if b, err := json.Marshal(t); err == nil {
			if forms := numberForms(string(b)); len(forms) > 0 {
				return forms
			}
		}
	case json.Number:
		if forms := numberForms(t.String()); len(forms) > 0 {
			return forms
		}
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return nil
	}
	return []string{"j:" + string(b)}
}

func numberForms(text string) []string {
	var forms []string
	for _, n := range store.NumericClaimForms(text) {
		forms = append(forms, "n:"+n)
	}
	return forms
}

// refuseAppAuthoredReferences refuses any pad-attachment: token, in any
// spelling, in an app artifact's content or field values, and any
// workspace-qualified wiki link in its content.
func refuseAppAuthoredReferences(content string, fields map[string]any) error {
	if strings.Contains(strings.ToLower(content), "pad-attachment:") {
		return errors.New("pad-attachment: references are not accepted in app content")
	}
	// Fail closed: a field set that cannot be serialized cannot be checked,
	// so it is refused, never waved through. Unreachable from a preview today
	// (fields come from decoded JSON, which always re-encodes); the direct
	// test reaches it.
	b, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("app content fields cannot be checked: %w", err)
	}
	if strings.Contains(strings.ToLower(string(b)), "pad-attachment:") {
		return errors.New("pad-attachment: references are not accepted in app content")
	}
	for _, l := range links.ExtractWikiLinks(content) {
		if l.Kind == links.WikiLinkKindWorkspaceRef {
			return errors.New("workspace-qualified links are not accepted in app content")
		}
	}
	return nil
}
