package models

import (
	"encoding/json"
	"errors"
	"strings"
)

// PLAN-3535: a collection either tracks WORK (its items have a lifecycle and
// finish) or holds REFERENCE material (its items just exist: docs,
// conventions, playbooks). A reference collection's items do not count toward
// parent progress, do not block a parent from closing, and stay out of
// Insights and the agent/dashboard open-work counts. They still count in the
// sidebar, search, links and their own collection list.
//
// Stored as CollectionSettings.TracksWork (JSON `tracks_work`). ABSENT means
// work, today's behaviour, so an old row, an API writer that never sends the
// key, and a client/server skew all keep counting as before. Every reader
// goes through CollectionTracksWork / CollectionTracksWorkJSON; nothing reads
// the key itself.

// CollectionTracksWork reports whether a collection's items count as work.
func CollectionTracksWork(settings CollectionSettings) bool {
	return settings.TracksWork == nil || *settings.TracksWork
}

// CollectionTracksWorkJSON is CollectionTracksWork over the stored settings
// JSON. Unparseable settings count as work (today's behaviour).
func CollectionTracksWorkJSON(settingsJSON string) bool {
	if strings.TrimSpace(settingsJSON) == "" {
		return true
	}
	var s CollectionSettings
	if err := json.Unmarshal([]byte(settingsJSON), &s); err != nil {
		return true
	}
	return CollectionTracksWork(s)
}

// DefaultTracksWork is the value a NEW collection gets when its creator did
// not say: a system collection (Conventions, Playbooks) is reference; any
// other collection is work when its done field is a select that declares
// terminal options (its items can finish), and reference otherwise.
func DefaultTracksWork(schemaJSON, settingsJSON string, isSystem bool) bool {
	if isSystem {
		return false
	}
	var schema CollectionSchema
	if err := UnmarshalItemFieldSchema([]byte(schemaJSON), &schema); err != nil {
		return true
	}
	var settings CollectionSettings
	_ = json.Unmarshal([]byte(settingsJSON), &settings)
	key := DoneFieldKey(schema, settings)
	for _, f := range schema.Fields {
		if f.Key == key && f.Type == "select" && len(f.TerminalOptions) > 0 {
			return true
		}
	}
	return false
}

// WithTracksWorkDefault returns settingsJSON with `tracks_work` set to
// DefaultTracksWork when the creator did not set it; settings that already
// carry the key, or that do not parse as an object, are returned unchanged.
// Used on collection CREATE only (not import, which keeps what it carries).
func WithTracksWorkDefault(settingsJSON, schemaJSON string, isSystem bool) string {
	raw := strings.TrimSpace(settingsJSON)
	if raw == "" {
		raw = "{}"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return settingsJSON
	}
	if _, ok := m["tracks_work"]; ok {
		return settingsJSON
	}
	v := "false"
	if DefaultTracksWork(schemaJSON, raw, isSystem) {
		v = "true"
	}
	m["tracks_work"] = json.RawMessage(v)
	out, err := json.Marshal(m)
	if err != nil {
		return settingsJSON
	}
	return string(out)
}

// CarryTracksWork returns newSettingsJSON with the stored `tracks_work` value
// carried over when the new settings do not mention the key (PLAN-3535).
// Settings are written wholesale, and every client written before the key
// existed rebuilds them without it, so absence on UPDATE means "not
// mentioned", never "reset to work". A new value that does set the key wins.
// Anything that does not parse as an object is returned unchanged.
func CarryTracksWork(newSettingsJSON, storedSettingsJSON string) string {
	var stored map[string]json.RawMessage
	if err := json.Unmarshal([]byte(storedSettingsJSON), &stored); err != nil {
		return newSettingsJSON
	}
	v, ok := stored["tracks_work"]
	if !ok {
		return newSettingsJSON
	}
	raw := strings.TrimSpace(newSettingsJSON)
	if raw == "" {
		raw = "{}"
	}
	var next map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &next); err != nil || next == nil {
		return newSettingsJSON
	}
	if _, has := next["tracks_work"]; has {
		return newSettingsJSON
	}
	next["tracks_work"] = v
	out, err := json.Marshal(next)
	if err != nil {
		return newSettingsJSON
	}
	return string(out)
}

// ValidateTracksWorkSetting refuses a `tracks_work` that is present but not a
// JSON boolean. An absent key is fine (it means work on create, "unchanged"
// on update).
func ValidateTracksWorkSetting(settingsJSON string) error {
	raw := strings.TrimSpace(settingsJSON)
	if raw == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil // the settings-shape check owns that error
	}
	v, ok := m["tracks_work"]
	if !ok {
		return nil
	}
	switch strings.TrimSpace(string(v)) {
	case "true", "false":
		return nil
	}
	return errTracksWorkNotBool
}

var errTracksWorkNotBool = errors.New("settings.tracks_work must be true or false")

// SetTracksWork returns settingsJSON with tracks_work set to v and every other
// key unchanged. Settings that do not parse as an object are an error: the
// caller asked for a value it cannot store.
func SetTracksWork(settingsJSON string, v bool) (string, error) {
	raw := strings.TrimSpace(settingsJSON)
	if raw == "" {
		raw = "{}"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return "", errors.New("the collection's settings are not a JSON object; tracks_work cannot be set")
	}
	if v {
		m["tracks_work"] = json.RawMessage("true")
	} else {
		m["tracks_work"] = json.RawMessage("false")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
