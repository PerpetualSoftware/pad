// Package redisns builds Pad's Redis key and channel names from a single
// per-installation namespace (BUG-2724).
//
// THE PROBLEM IT SOLVES. Every Redis name Pad uses was flat — pad:events:,
// pad:event_seq, pad:watchevents*, pad:session:* — so two Pad installations
// pointed at one Redis endpoint cross-feed each other's notifications and
// merge each other's session-presence registries. Selecting different DB
// numbers only half helps: ordinary keys are DB-scoped, so the presence
// registries would stay separate — but Redis pub/sub is not namespaced by
// DB at all, so the buses cross-feed regardless, and a channel is where a
// notification actually travels. The
// blast radius is narrower than it sounds (delivery is filtered per-caller
// on user id, and user ids are per-installation UUIDs) but for a CLONED
// database — a staging environment restored from a production dump — it is
// a genuine cross-tenant leak.
//
// WHY ONE PACKAGE RATHER THAN A PREFIX PER FILE. internal/watchevents'
// existing ruling is that a prefix belongs to all keyspaces at once or to
// none, because an operator rule that holds for two of three keyspaces is
// harder to state than the flat one it replaces. This package is that
// "at once": one value, built once in cmd/pad/cmd_server.go, passed into
// all three constructors.
//
// That is a CONVENTION, not a guarantee — each constructor takes its own
// Keys and nothing in the type system stops a future caller passing a
// different one. cmd/pad/redis_keyspace_wiring_test.go enforces it by
// reading the wiring source, which is weaker than a compiler.
//
// HAZARD FOR ANYONE SWEEPING THE PREFIX. The string "pad:" also begins
// Pad's OAuth SCOPE values — pad:read, pad:write, pad:admin — in
// internal/store/connected_apps.go, internal/server/handlers_oauth.go,
// handlers_well_known.go and middleware_auth.go. Those are not Redis keys
// and rewriting them breaks authorization. That is exactly why names are
// built HERE, through a function, rather than assembled from a literal at
// each site where a grep-and-replace would find both.
//
// NOT CLUSTER SUPPORT. These names carry no hash tags, and Pad dials
// redis.NewClient rather than NewClusterClient, so Redis Cluster is
// unsupported. Adding tags is deliberately deferred to whenever a cluster
// client lands (BUG-2724): the buses' publish script spans four keys in a
// single EVAL and would fail CROSSSLOT exactly as presence's MGET does, so
// tagging one keyspace buys nothing on its own — and there is no cluster
// client here to exercise tagged keys against, which would make them
// untested by construction.
package redisns

import (
	"fmt"
	"strings"
)

// prefix is Pad's own root, present with or without a namespace so that a
// shared Redis's keyspace is still legible to an operator running KEYS.
const prefix = "pad:"

// reservedNames are Pad's own first path segments. A namespace equal to
// one of them nests this installation inside the DEFAULT installation's
// keyspace — "events" being the sharp case, since pad:events:* is the
// activity channel space. Nesting rather than an exact collision; see
// Parse for why it is refused regardless.
//
// Kept in sync by hand with the suffixes the three packages pass to
// Name.
var reservedNames = map[string]bool{
	"events":            true,
	"event_seq":         true,
	"watchevents":       true,
	"watchevents_seq":   true,
	"watchevents_epoch": true,
	"session":           true,
	"sessions":          true,
}

const reservedList = "events, event_seq, watchevents, watchevents_seq, watchevents_epoch, session, sessions"

// Keys builds namespaced names. The zero value is Default — today's exact
// names — which is what makes an existing deployment's replay buffers,
// counters and presence entries survive an upgrade untouched.
type Keys struct {
	ns string
}

// Default produces the historical, un-namespaced names. Equivalent to the
// zero Keys; named so a call site can say which it means.
var Default = Keys{}

// Parse validates a namespace and returns the Keys for it. An UNSET
// namespace — the empty string — is valid and yields Default.
//
// A value that is present but whitespace-only is an ERROR, not a
// synonym for unset (codex round 3). Trimming it to Default would mean
// PAD_REDIS_NAMESPACE=" " — which is what a broken template substitution
// produces — silently restores the historical keyspace and collides with
// the un-namespaced installation it was set to separate from. That is
// precisely the leak this package exists to prevent, arriving through
// the mechanism meant to prevent it. Nobody sets spaces on purpose, so
// there is no legitimate case to preserve, and a startup error is
// something an operator fixes in one minute.
//
// The character set is deliberately narrow — lowercase letters, digits,
// hyphen, underscore — because a namespace ends up inside key names, in
// Lua KEYS arguments, and in operator-facing logs. A colon is rejected
// specifically: it is Pad's own separator, so a namespace containing one
// spans segments and makes the keyspace ambiguous to anyone (or anything)
// reading KEYS output back.
//
// A second hazard needs no colon, and reservedNames below rejects it: a
// namespace equal to one of Pad's own first path segments NESTS this
// installation inside the default one's keyspace. Namespace "events"
// puts every key of this installation under pad:events:*, the default
// installation's activity channel space.
//
// That is prefix nesting, not an exact key collision — the default
// channels are pad:events:<workspace-uuid>, so a namespaced key would
// have to match one of those UUIDs to collide outright. Refused anyway,
// because a keyspace where one installation's keys live inside another's
// prefix cannot be reasoned about with KEYS, cleaned up, or told apart
// in a dump.
func Parse(ns string) (Keys, error) {
	if ns == "" {
		return Default, nil
	}
	if strings.TrimSpace(ns) == "" {
		return Keys{}, fmt.Errorf("redis namespace %q is whitespace only: leave it unset for the default keyspace, or give it a real name — a blank value would silently share the default keyspace with another installation", ns)
	}
	if reservedNames[ns] {
		return Keys{}, fmt.Errorf("redis namespace %q is one of Pad's own key segments (%s): it would place this installation's keys inside the default installation's keyspace", ns, reservedList)
	}
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return Keys{}, fmt.Errorf("redis namespace %q contains %q: only lowercase letters, digits, '-' and '_' are allowed", ns, string(r))
		}
	}
	return Keys{ns: ns}, nil
}

// Name returns the full Redis key or channel name for a suffix. The
// suffix is the historical name minus the "pad:" root — "event_seq",
// "watchevents", "events:" and so on.
//
//	Default.Name("event_seq")           == "pad:event_seq"
//	Keys{ns:"staging"}.Name("event_seq") == "pad:staging:event_seq"
func (k Keys) Name(suffix string) string {
	if k.ns == "" {
		return prefix + suffix
	}
	return prefix + k.ns + ":" + suffix
}

// Namespace returns the configured namespace, empty for Default. For log
// lines and health output — not for building names, which is Name's job.
func (k Keys) Namespace() string { return k.ns }
