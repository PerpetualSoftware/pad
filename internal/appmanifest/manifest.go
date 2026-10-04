// Package appmanifest parses and validates an app's apps/1 manifest (SPEC-6,
// DOC-3371 §1). It is pure: no network, no database. The fetch policy lives
// in internal/appfetch and the staging and provisioning in internal/store.
//
// The manifest is published at {base_url}/.well-known/pad-app.json and is a
// contract surface. Validation is strict: an unknown key is refused rather
// than ignored, so a manifest written for a later contract cannot install
// here with part of it silently dropped.
package appmanifest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Contract versions this server speaks.
const (
	AppsContract   = 1
	EventsContract = 1
)

// Caps (DOC-3371 §2 "Staging caps").
const (
	MaxManifestBytes     = 256 << 10
	MaxArtifacts         = 32
	MaxCollections       = 32
	MaxEvents            = 64
	MaxItemActions       = 32
	MaxRedirectURIs      = 16
	MaxConfigSchemaBytes = 64 << 10
	maxTextLen           = 2000
)

// WellKnownPath is where the manifest is published under base_url.
const WellKnownPath = "/.well-known/pad-app.json"

// SubscribableEvents are the v1 events an app may subscribe to: those whose
// subject resolves to exactly one companion item or comment (§5).
var SubscribableEvents = map[string]bool{
	"item.created": true, "item.updated": true, "item.status_changed": true,
	"item.deleted": true, "item.restored": true,
	"comment.created": true, "comment.updated": true, "comment.deleted": true,
}

// Manifest is a parsed, validated apps/1 manifest.
type Manifest struct {
	ID            string          `json:"id"`
	Version       string          `json:"version"`
	MinContract   MinContract     `json:"min_contract"`
	Title         string          `json:"title"`
	Description   string          `json:"description"`
	Publisher     string          `json:"publisher"`
	Homepage      string          `json:"homepage"`
	BaseURL       string          `json:"base_url"`
	RedirectURIs  []string        `json:"redirect_uris"`
	Scopes        Scopes          `json:"scopes"`
	Events        []Event         `json:"events"`
	WebhookURL    string          `json:"webhook_url"`
	CompanionPack CompanionPack   `json:"companion_pack"`
	ItemActions   []ItemAction    `json:"item_actions"`
	ConfigSchema  json.RawMessage `json:"config_schema"`
	Docs          string          `json:"docs"`

	// Origin is the normalized origin of BaseURL: the app's identity.
	Origin string `json:"-"`
}

type MinContract struct {
	Apps   int `json:"apps"`
	Events int `json:"events"`
}

type Scopes struct {
	Service   Access `json:"service"`
	Delegated Access `json:"delegated"`
}

type Access struct {
	Access string `json:"access"`
}

type Event struct {
	Name        string   `json:"name"`
	Collections []string `json:"collections"`
}

type CompanionPack struct {
	Collections []Collection `json:"collections"`
	Artifacts   []Artifact   `json:"artifacts"`
}

type Collection struct {
	Key    string          `json:"key"`
	Slug   string          `json:"slug"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`

	// Parsed is Schema, parsed.
	Parsed models.CollectionSchema `json:"-"`
}

type Artifact struct {
	Key    string `json:"key"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type ItemAction struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Collections []string `json:"collections"`
	Path        string   `json:"path"`
}

// Error is one validation failure, at a JSON path in the manifest.
type Error struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string { return "app manifest: " + e.Path + ": " + e.Reason }

func fail(path, format string, args ...any) error {
	return &Error{Path: path, Reason: fmt.Sprintf(format, args...)}
}

// IsError reports whether err is a manifest validation failure.
func IsError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

var (
	keyRE    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	slugRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`)
	idRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[a-z0-9][a-z0-9._-]{0,63}$`)
	semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parse decodes and validates manifest bytes. The first failure is returned
// as an *Error naming its path.
func Parse(data []byte) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, fail("$", "manifest exceeds %d KiB", MaxManifestBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fail("$", "not a valid apps/1 manifest: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fail("$", "trailing data after the manifest")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if !idRE.MatchString(m.ID) {
		return fail("id", "must be publisher/name in lower case")
	}
	if !semverRE.MatchString(m.Version) {
		return fail("version", "must be a semantic version")
	}
	if m.MinContract.Apps < 1 || m.MinContract.Apps > AppsContract {
		return fail("min_contract.apps", "this server speaks apps/%d", AppsContract)
	}
	if m.MinContract.Events < 1 || m.MinContract.Events > EventsContract {
		return fail("min_contract.events", "this server speaks events/%d", EventsContract)
	}
	for _, f := range []struct{ path, v string }{{"title", m.Title}, {"publisher", m.Publisher}} {
		if strings.TrimSpace(f.v) == "" {
			return fail(f.path, "is required")
		}
	}
	for _, f := range []struct{ path, v string }{{"title", m.Title}, {"description", m.Description}, {"publisher", m.Publisher}} {
		if len(f.v) > maxTextLen {
			return fail(f.path, "is longer than %d characters", maxTextLen)
		}
	}

	origin, err := NormalizeOrigin(m.BaseURL)
	if err != nil {
		return fail("base_url", "%v", err)
	}
	m.Origin = origin
	if m.Homepage != "" {
		if err := m.under(m.Homepage); err != nil {
			return fail("homepage", "%v", err)
		}
	}
	if len(m.RedirectURIs) == 0 || len(m.RedirectURIs) > MaxRedirectURIs {
		return fail("redirect_uris", "between 1 and %d are required", MaxRedirectURIs)
	}
	for i, u := range m.RedirectURIs {
		if err := m.under(u); err != nil {
			return fail(fmt.Sprintf("redirect_uris[%d]", i), "%v", err)
		}
	}
	for _, f := range []struct{ path, v string }{{"scopes.service.access", m.Scopes.Service.Access}, {"scopes.delegated.access", m.Scopes.Delegated.Access}} {
		if f.v != "read" && f.v != "write" {
			return fail(f.path, "must be read or write")
		}
	}
	if m.WebhookURL != "" {
		if err := m.under(m.WebhookURL); err != nil {
			return fail("webhook_url", "%v", err)
		}
	}
	if m.Docs != "" {
		if err := m.under(m.Docs); err != nil {
			return fail("docs", "%v", err)
		}
	}

	// Companion pack.
	pack := m.CompanionPack
	if len(pack.Collections) == 0 || len(pack.Collections) > MaxCollections {
		return fail("companion_pack.collections", "between 1 and %d are required", MaxCollections)
	}
	keys := map[string]bool{}
	slugs := map[string]bool{}
	for i := range pack.Collections {
		c := &m.CompanionPack.Collections[i]
		p := fmt.Sprintf("companion_pack.collections[%d]", i)
		if !keyRE.MatchString(c.Key) {
			return fail(p+".key", "must be a lower-case key")
		}
		if keys[c.Key] {
			return fail(p+".key", "duplicate key %q", c.Key)
		}
		keys[c.Key] = true
		if !slugRE.MatchString(c.Slug) {
			return fail(p+".slug", "must be a lower-case slug")
		}
		if slugs[c.Slug] {
			return fail(p+".slug", "duplicate slug %q", c.Slug)
		}
		slugs[c.Slug] = true
		if len(c.Name) > 200 {
			return fail(p+".name", "is longer than 200 characters")
		}
		if len(c.Schema) == 0 {
			return fail(p+".schema", "is required")
		}
		// Strict, like the manifest itself: a misspelled key ("requried")
		// would otherwise be dropped silently, losing what it meant (codex
		// round 2).
		sdec := json.NewDecoder(bytes.NewReader(c.Schema))
		sdec.DisallowUnknownFields()
		if err := sdec.Decode(&c.Parsed); err != nil {
			return fail(p+".schema", "is not a collection schema: %v", err)
		}
	}
	if len(pack.Artifacts) > MaxArtifacts {
		return fail("companion_pack.artifacts", "at most %d", MaxArtifacts)
	}
	artKeys := map[string]bool{}
	for i, a := range pack.Artifacts {
		p := fmt.Sprintf("companion_pack.artifacts[%d]", i)
		if !keyRE.MatchString(a.Key) {
			return fail(p+".key", "must be a lower-case key")
		}
		if artKeys[a.Key] {
			return fail(p+".key", "duplicate key %q", a.Key)
		}
		artKeys[a.Key] = true
		if err := m.under(a.URL); err != nil {
			return fail(p+".url", "%v", err)
		}
		if !sha256RE.MatchString(a.SHA256) {
			return fail(p+".sha256", "must be 64 lower-case hex characters")
		}
	}

	// Events: v1-subscribable names, companion keys only.
	if len(m.Events) > MaxEvents {
		return fail("events", "at most %d", MaxEvents)
	}
	if len(m.Events) > 0 && m.WebhookURL == "" {
		return fail("webhook_url", "is required when events are declared")
	}
	for i, e := range m.Events {
		p := fmt.Sprintf("events[%d]", i)
		if !SubscribableEvents[e.Name] {
			return fail(p+".name", "%q is not subscribable in apps/1", e.Name)
		}
		if len(e.Collections) == 0 {
			return fail(p+".collections", "at least one companion collection key is required")
		}
		for j, k := range e.Collections {
			if !keys[k] {
				return fail(fmt.Sprintf("%s.collections[%d]", p, j), "%q is not a companion collection key", k)
			}
		}
	}

	// Item actions.
	if len(m.ItemActions) > MaxItemActions {
		return fail("item_actions", "at most %d", MaxItemActions)
	}
	actKeys := map[string]bool{}
	for i, a := range m.ItemActions {
		p := fmt.Sprintf("item_actions[%d]", i)
		if !keyRE.MatchString(a.Key) {
			return fail(p+".key", "must be a lower-case key")
		}
		if actKeys[a.Key] {
			return fail(p+".key", "duplicate key %q", a.Key)
		}
		actKeys[a.Key] = true
		if strings.TrimSpace(a.Label) == "" || len(a.Label) > 80 {
			return fail(p+".label", "is required, at most 80 characters")
		}
		if len(a.Collections) == 0 {
			return fail(p+".collections", "at least one companion collection key is required")
		}
		for j, k := range a.Collections {
			if !keys[k] {
				return fail(fmt.Sprintf("%s.collections[%d]", p, j), "%q is not a companion collection key", k)
			}
		}
		if !absolutePath(a.Path) {
			return fail(p+".path", "must be an absolute path under base_url")
		}
	}

	// Config schema: a JSON object, size-capped; values are owner-edited and
	// never secrets (§1). It is not evaluated here.
	if len(m.ConfigSchema) > 0 {
		if len(m.ConfigSchema) > MaxConfigSchemaBytes {
			return fail("config_schema", "exceeds %d KiB", MaxConfigSchemaBytes>>10)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(m.ConfigSchema, &obj); err != nil {
			return fail("config_schema", "must be a JSON Schema object")
		}
	}
	return nil
}

// NormalizeOrigin returns the canonical origin of an https base URL:
// lower-case host, no default port, and no path, query, fragment or
// userinfo. It is the app's identity (DOC-3371 Definitions).
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("is not a URL")
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("must be https")
	}
	if u.User != nil {
		return "", fmt.Errorf("must not carry credentials")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("must have a host")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("must be an origin, with no path, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "443" {
		port = ""
	}
	if port != "" {
		return "https://" + joinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		return "https://[" + host + "]", nil
	}
	return "https://" + host, nil
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// under requires raw to be an https URL on exactly the manifest's origin.
func (m *Manifest) under(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("must be an https URL under base_url")
	}
	if u.User != nil {
		return fmt.Errorf("must not carry credentials")
	}
	o, err := NormalizeOrigin("https://" + u.Host)
	if err != nil || o != m.Origin {
		return fmt.Errorf("must be under base_url (%s)", m.Origin)
	}
	return nil
}

// absolutePath accepts a path to join onto the origin: it starts with one
// "/", and has no scheme, authority, backslash or control character. A
// "//host/x" would keep the origin when concatenated but name another host
// when resolved as a reference, so it is refused outright.
func absolutePath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || len(p) > 1024 {
		return false
	}
	for _, r := range p {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil && u.Opaque == ""
}

// ManifestURL is where a base URL's manifest is published.
func ManifestURL(origin string) string { return origin + WellKnownPath }

// SHA256Hex is the lower-case hex sha256 the manifest and digests use.
func SHA256Hex(sum [32]byte) string { return hex.EncodeToString(sum[:]) }
