package collections

import (
	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Bootstrap payload keys — the `key` half of a bootstrap_include declaration
// (SPEC-5 §Collection traits). A key names the payload a declaration feeds, so
// these are the canonical names of the FIRST-PARTY boot surfaces.
//
// They are the same three payloads the pre-trait bootstrap hardcoded, kept as
// first-party views rather than replaced by a purely generic array: the
// binding day-45 ruling was that the boot MECHANISM goes generic, which the
// trait declarations deliver, and nothing in it required breaking the CLI,
// skill, MCP, and web consumers that read these keys. Any collection may now
// declare an include feeding any key, including a new one of its own.
// Aliases of the grammar constants in models, kept so callers in this package
// and its consumers read naturally. The canonical definitions live with the
// trait grammar because validation must know them — see
// models.firstPartyKeyModes.
const (
	// BootstrapKeyConventions carries full bodies of the always-on rules.
	BootstrapKeyConventions = models.BootstrapKeyConventions
	// BootstrapKeyConventionIndex carries body-less metadata for every active
	// convention, so triggered rules are discoverable without their bodies.
	BootstrapKeyConventionIndex = models.BootstrapKeyConventionIndex
	// BootstrapKeyPlaybooks carries playbook metadata.
	BootstrapKeyPlaybooks = models.BootstrapKeyPlaybooks
)

// TraitedCollection pairs a stored collection's identity with its parsed
// kernel traits. Consumers resolve behavior by asking which collection
// DECLARES a trait rather than by naming a slug.
type TraitedCollection struct {
	ID     string
	Slug   string
	Traits models.CollectionTraits
}

// FindByArtifactKind returns the collection declaring the given artifact kind.
// Returns nil when no collection declares it — an unknown kind imports as a
// plain item rather than failing (SPEC-5 §artifact_kind).
//
// When several collections declare the same kind the first in the supplied
// order wins. Do NOT read that as a defined tie-break: ListTraitedCollections
// orders by sort_order then created_at, and template-seeded collections share
// both, so which one wins is effectively arbitrary. Two collections declaring
// one kind is a misconfiguration, not a supported shape — SPEC-0 L6 wants it
// rejected at install time, which is pack-installer territory (phase 3) and
// not reachable from here. Until then this resolves to *a* collection rather
// than failing, because refusing to export an item is worse than exporting it
// under one of two identical declarations.
func FindByArtifactKind(colls []TraitedCollection, kind string) *TraitedCollection {
	if kind == "" {
		return nil
	}
	for i := range colls {
		if colls[i].Traits.ArtifactKind != nil && colls[i].Traits.ArtifactKind.Kind == kind {
			return &colls[i]
		}
	}
	return nil
}

// FindByInvocationField returns the collections that route by invocation slug
// — those declaring the invocation_field trait, in the supplied order.
func FindByInvocationField(colls []TraitedCollection) []TraitedCollection {
	var out []TraitedCollection
	for _, c := range colls {
		if c.Traits.InvocationField != "" {
			out = append(out, c)
		}
	}
	return out
}

// FindBootstrapIncludes returns every (collection, declaration) pair feeding
// the named bootstrap payload. More than one collection MAY feed the same key
// — that is the mechanism by which a pack contributes to an existing boot
// surface instead of shadowing it (SPEC-5's slots-coexist posture applied to
// boot payloads).
func FindBootstrapIncludes(colls []TraitedCollection, key string) []BootstrapSource {
	var out []BootstrapSource
	for _, c := range colls {
		if inc := c.Traits.BootstrapIncludeForKey(key); inc != nil {
			out = append(out, BootstrapSource{Collection: c, Include: *inc})
		}
	}
	return out
}

// BootstrapSource is one collection's declaration feeding one boot payload.
type BootstrapSource struct {
	Collection TraitedCollection
	Include    models.BootstrapInclude
}

// TraitedFromCollections adapts stored collection models into the trait view.
// Lets callers that already hold []models.Collection (CLI, in-process HTTP
// dispatchers) reuse the same lookups as the store-side path without a second
// round-trip. A collection whose traits blob doesn't parse contributes an
// empty declaration set rather than failing the batch — same degradation rule
// as Store.ListTraitedCollections.
func TraitedFromCollections(colls []models.Collection) []TraitedCollection {
	out := make([]TraitedCollection, 0, len(colls))
	for _, c := range colls {
		traits, err := models.ParseCollectionTraits(c.Traits)
		if err != nil {
			traits = models.CollectionTraits{}
		}
		out = append(out, TraitedCollection{ID: c.ID, Slug: c.Slug, Traits: traits})
	}
	return out
}

// SlugForArtifactKind returns the slug of the collection declaring the given
// artifact kind, or "" when none does.
func SlugForArtifactKind(colls []models.Collection, kind string) string {
	if c := FindByArtifactKind(TraitedFromCollections(colls), kind); c != nil {
		return c.Slug
	}
	return ""
}

// CanonicalTraitsForSlug returns the kernel-trait declarations that a
// collection with the given slug carries by default, or the zero value when
// the slug has no canonical declarations.
//
// This is the ONE definition of "what conventions/playbooks declare", shared by
// three surfaces that would otherwise drift: the template definitions (new
// workspaces), the workspace-import compatibility inference (archives written
// before traits existed), and — pinned by test rather than shared directly,
// since it is SQL — the migration backfill for existing workspaces.
//
// Slug-keyed, and necessarily so: it is only ever consulted for rows that
// carry NO declarations, where the slug is the only evidence of intent that
// exists. Once a collection declares traits, the declaration is authoritative
// and this is never asked. TASK-2657.
func CanonicalTraitsForSlug(slug string) models.CollectionTraits {
	switch slug {
	case "conventions":
		return models.CollectionTraits{
			BootstrapInclude: []models.BootstrapInclude{
				{
					Mode:   models.BootstrapModeBodies,
					Filter: map[string]string{"status": "active", "trigger": "always"},
					Key:    BootstrapKeyConventions,
				},
				{
					Mode:   models.BootstrapModeMetadata,
					Filter: map[string]string{"status": "active"},
					Key:    BootstrapKeyConventionIndex,
				},
			},
			ArtifactKind: &models.ArtifactKindTrait{Kind: string(artifact.KindConvention)},
		}
	case "playbooks":
		return models.CollectionTraits{
			BootstrapInclude: []models.BootstrapInclude{
				{Mode: models.BootstrapModeMetadata, Key: BootstrapKeyPlaybooks},
			},
			InvocationField: models.InvocationSlugField,
			ArtifactKind:    &models.ArtifactKindTrait{Kind: string(artifact.KindPlaybook)},
		}
	}
	return models.CollectionTraits{}
}
