package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// IDEA-3906 / TASK-3919: an awaits_human collection holds ASKS of a person
// (a human-tasks or asks collection). The needs-you router treats an open,
// assigned item in such a collection as rule 1 "asked".
//
// Stored as the settings key `awaits_human`. ABSENT means false, so every existing collection, an API writer that never sends the key,
// and a client/server skew all read as an ordinary collection. No migration
// marks existing collections (IDEA-3906 A3, default: set per workspace).
// Stored, carried and validated exactly like tracks_work (tracks_work.go),
// but it is not a CollectionSettings field: it is read on its own, so a
// stored non-boolean reads as false here and breaks no other setting.

// CollectionAwaitsHumanJSON reports whether a collection's items are asks of
// a person, from the stored settings JSON. Anything but a JSON true reads as
// false, unparseable settings included.
func CollectionAwaitsHumanJSON(settingsJSON string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsJSON), &m); err != nil {
		return false
	}
	return strings.TrimSpace(string(m["awaits_human"])) == "true"
}

// CarryAwaitsHuman returns newSettingsJSON with the stored `awaits_human`
// value carried over when the new settings do not mention the key. Settings
// are written wholesale and a client written before the key existed rebuilds
// them without it, so absence on UPDATE means "not mentioned", never "reset".
func CarryAwaitsHuman(newSettingsJSON, storedSettingsJSON string) string {
	return carryBoolSetting(newSettingsJSON, storedSettingsJSON, "awaits_human")
}

// ValidateAwaitsHumanSetting refuses an `awaits_human` that is present but not
// a JSON boolean. An absent key is fine.
func ValidateAwaitsHumanSetting(settingsJSON string) error {
	if !boolSettingValid(settingsJSON, "awaits_human") {
		return errAwaitsHumanNotBool
	}
	return nil
}

var errAwaitsHumanNotBool = errors.New("settings.awaits_human must be true or false")

// SetAwaitsHuman returns settingsJSON with awaits_human set to v and every
// other key unchanged. Settings that do not parse as an object are an error.
func SetAwaitsHuman(settingsJSON string, v bool) (string, error) {
	return setBoolSetting(settingsJSON, "awaits_human", v)
}

// carryBoolSetting copies key from storedSettingsJSON into newSettingsJSON
// when the stored settings carry it and the new ones do not. Anything that
// does not parse as an object is returned unchanged.
func carryBoolSetting(newSettingsJSON, storedSettingsJSON, key string) string {
	var stored map[string]json.RawMessage
	if err := json.Unmarshal([]byte(storedSettingsJSON), &stored); err != nil {
		return newSettingsJSON
	}
	v, ok := stored[key]
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
	if _, has := next[key]; has {
		return newSettingsJSON
	}
	next[key] = v
	out, err := json.Marshal(next)
	if err != nil {
		return newSettingsJSON
	}
	return string(out)
}

// boolSettingValid reports false only when key is present and not a JSON
// boolean. Unparseable settings are left to the settings-shape check.
func boolSettingValid(settingsJSON, key string) bool {
	raw := strings.TrimSpace(settingsJSON)
	if raw == "" {
		return true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return true
	}
	v, ok := m[key]
	if !ok {
		return true
	}
	switch strings.TrimSpace(string(v)) {
	case "true", "false":
		return true
	}
	return false
}

// ErrSettingsNotObject is wrapped by SetTracksWork and SetAwaitsHuman when the
// settings they merge into do not parse as a JSON object, so a handler can
// answer the caller 400 instead of 500 (BUG-3976). A full settings write
// replaces such a row; a typed member alone does not rewrite the rest.
var ErrSettingsNotObject = errors.New("the collection's settings are not a JSON object")

// CheckSettingsObject refuses a settings value that is not a JSON object,
// wrapping ErrSettingsNotObject. "" is the empty sentinel the store writes as
// "{}", so it passes. The HTTP decoder already refuses the rest (IDEA-1488);
// this is the same rule at the store doors, for Go callers that never pass
// through it (BUG-3985).
func CheckSettingsObject(settingsJSON string) error {
	if settingsJSON == "" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsJSON), &m); err != nil || m == nil {
		return ErrSettingsNotObject
	}
	return nil
}

// setBoolSetting returns settingsJSON with key set to v and every other key
// unchanged.
func setBoolSetting(settingsJSON, key string, v bool) (string, error) {
	raw := strings.TrimSpace(settingsJSON)
	if raw == "" {
		raw = "{}"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return "", fmt.Errorf("%w; %s cannot be set", ErrSettingsNotObject, key)
	}
	if v {
		m[key] = json.RawMessage("true")
	} else {
		m[key] = json.RawMessage("false")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
