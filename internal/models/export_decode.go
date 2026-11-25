package models

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/PerpetualSoftware/pad/internal/jsonscan"
)

// ExportDecodeChecks lets callers validate or transform each top-level member
// name and each streamed record (array element or whole non-slice member) as
// DecodeWorkspaceExport reads an export stream.
type ExportDecodeChecks interface {
	MemberName(name string) error
	Record(member string, raw []byte) ([]byte, error)
}

var workspaceExportFoldedFields = map[string]string{
	"VERSION":          "version",
	"EXPORTED_AT":      "exported_at",
	"WORKSPACE":        "workspace",
	"COLLECTIONS":      "collections",
	"ITEMS":            "items",
	"COMMENTS":         "comments",
	"ITEM_LINKS":       "item_links",
	"ITEM_VERSIONS":    "item_versions",
	"REMINDERS":        "reminders",
	"VERIFICATIONS":    "verifications",
	"SCHEDULES":        "schedules",
	"SCHEDULE_CHANGES": "schedule_changes",
	"SCHEDULE_LINKS":   "schedule_links",
	"SCHEDULE_GATES":   "schedule_gates",
}

func matchWorkspaceExportField(name string) string {
	switch name {
	case "version", "exported_at", "workspace",
		"collections", "items", "comments", "item_links", "item_versions",
		"reminders", "verifications", "schedules",
		"schedule_changes", "schedule_links", "schedule_gates":
		return name
	}
	return workspaceExportFoldedFields[jsonscan.FoldName([]byte(name))]
}

func isWorkspaceExportSliceField(field string) bool {
	switch field {
	case "collections", "items", "comments", "item_links", "item_versions",
		"reminders", "verifications", "schedules",
		"schedule_changes", "schedule_links", "schedule_gates":
		return true
	}
	return false
}

// DecodeWorkspaceExport decodes a workspace export from r, streaming known
// top-level slice fields element by element so the whole raw JSON body does
// not need to be buffered at once (TASK-4839).
func DecodeWorkspaceExport(r io.Reader, c ExportDecodeChecks) (*WorkspaceExport, error) {
	dec := json.NewDecoder(r)

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if tok == nil {
		if err := expectExportEOF(dec); err != nil {
			return nil, err
		}
		return &WorkspaceExport{}, nil
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("invalid JSON: expected JSON object, got %v", tok)
	}

	var x WorkspaceExport
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("invalid JSON: expected object member name, got %v", keyTok)
		}
		if c != nil {
			if err := c.MemberName(key); err != nil {
				return nil, err
			}
		}

		matched := matchWorkspaceExportField(key)
		if isWorkspaceExportSliceField(matched) {
			if err := decodeExportSliceMember(dec, &x, key, matched, c); err != nil {
				return nil, err
			}
			continue
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		if c != nil {
			raw, err = c.Record(key, raw)
			if err != nil {
				return nil, err
			}
		}
		switch matched {
		case "version":
			if err := json.Unmarshal(raw, &x.Version); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}
		case "exported_at":
			if err := json.Unmarshal(raw, &x.ExportedAt); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}
		case "workspace":
			if err := json.Unmarshal(raw, &x.Workspace); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}
		}
	}

	endTok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if endTok != json.Delim('}') {
		return nil, fmt.Errorf("invalid JSON: expected '}', got %v", endTok)
	}
	if err := expectExportEOF(dec); err != nil {
		return nil, err
	}
	return &x, nil
}

func expectExportEOF(dec *json.Decoder) error {
	extra, err := dec.Token()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return fmt.Errorf("invalid JSON: unexpected trailing data %v", extra)
}

func markSlicePresence(x *WorkspaceExport, field string) {
	switch field {
	case "schedule_changes":
		x.hasV2Members = true
	case "schedule_links":
		x.hasV3Members = true
	case "schedule_gates":
		x.hasV4Members = true
	}
}

func decodeExportSliceMember(dec *json.Decoder, x *WorkspaceExport, wireKey, field string, c ExportDecodeChecks) error {
	markSlicePresence(x, field)

	valTok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if valTok == nil {
		if c != nil {
			if _, err := c.Record(wireKey, []byte("null")); err != nil {
				return err
			}
		}
		clearExportSliceField(x, field)
		return nil
	}
	if valTok != json.Delim('[') {
		return fmt.Errorf("invalid JSON: cannot unmarshal %v into Go struct field WorkspaceExport.%s", valTok, field)
	}

	initExportSliceField(x, field)

	for dec.More() {
		var elemRaw json.RawMessage
		if err := dec.Decode(&elemRaw); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		if c != nil {
			elemRaw, err = c.Record(wireKey, elemRaw)
			if err != nil {
				return err
			}
		}
		if err := appendExportSliceElem(x, field, elemRaw); err != nil {
			return err
		}
	}

	endTok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if endTok != json.Delim(']') {
		return fmt.Errorf("invalid JSON: expected ']', got %v", endTok)
	}
	return nil
}

func clearExportSliceField(x *WorkspaceExport, field string) {
	switch field {
	case "collections":
		x.Collections = nil
	case "items":
		x.Items = nil
	case "comments":
		x.Comments = nil
	case "item_links":
		x.ItemLinks = nil
	case "item_versions":
		x.ItemVersions = nil
	case "reminders":
		x.Reminders = nil
	case "verifications":
		x.Verifications = nil
	case "schedules":
		x.Schedules = nil
	case "schedule_changes":
		x.ScheduleChanges = nil
	case "schedule_links":
		x.ScheduleLinks = nil
	case "schedule_gates":
		x.ScheduleGates = nil
	}
}

func initExportSliceField(x *WorkspaceExport, field string) {
	switch field {
	case "collections":
		x.Collections = make([]CollectionExport, 0)
	case "items":
		x.Items = make([]ItemExport, 0)
	case "comments":
		x.Comments = make([]CommentExport, 0)
	case "item_links":
		x.ItemLinks = make([]ItemLinkExport, 0)
	case "item_versions":
		x.ItemVersions = make([]ItemVersionExport, 0)
	case "reminders":
		x.Reminders = make([]ReminderExport, 0)
	case "verifications":
		x.Verifications = make([]VerificationExport, 0)
	case "schedules":
		x.Schedules = make([]ScheduleExport, 0)
	case "schedule_changes":
		x.ScheduleChanges = make([]ScheduleChangeExport, 0)
	case "schedule_links":
		x.ScheduleLinks = make([]ScheduleLinkExport, 0)
	case "schedule_gates":
		x.ScheduleGates = make([]ScheduleGateExport, 0)
	}
}

func appendExportSliceElem(x *WorkspaceExport, field string, elemRaw []byte) error {
	switch field {
	case "collections":
		var v CollectionExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.Collections = append(x.Collections, v)
	case "items":
		var v ItemExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		if itemElementHasDeletedAt(elemRaw) {
			x.hasV4Members = true
		}
		x.Items = append(x.Items, v)
	case "comments":
		var v CommentExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.Comments = append(x.Comments, v)
	case "item_links":
		var v ItemLinkExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.ItemLinks = append(x.ItemLinks, v)
	case "item_versions":
		var v ItemVersionExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.ItemVersions = append(x.ItemVersions, v)
	case "reminders":
		var v ReminderExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.Reminders = append(x.Reminders, v)
	case "verifications":
		var v VerificationExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.Verifications = append(x.Verifications, v)
	case "schedules":
		var v ScheduleExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		hasV2, err := scheduleElementHasV2Members(elemRaw)
		if err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		if hasV2 {
			x.hasV2Members = true
		}
		x.Schedules = append(x.Schedules, v)
	case "schedule_changes":
		var v ScheduleChangeExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.ScheduleChanges = append(x.ScheduleChanges, v)
	case "schedule_links":
		var v ScheduleLinkExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.ScheduleLinks = append(x.ScheduleLinks, v)
	case "schedule_gates":
		var v ScheduleGateExport
		if err := json.Unmarshal(elemRaw, &v); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		x.ScheduleGates = append(x.ScheduleGates, v)
	}
	return nil
}

func itemElementHasDeletedAt(raw []byte) bool {
	var probe struct {
		DeletedAt json.RawMessage `json:"deleted_at"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.DeletedAt != nil
}

func scheduleElementHasV2Members(raw []byte) (bool, error) {
	var sc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sc); err != nil {
		return false, err
	}
	hasV2 := false
	for _, key := range []string{"id", "roles", "overrides", "forecast", "forecast_events", "children", "completion_evidence"} {
		if _, ok := sc[key]; ok {
			hasV2 = true
		}
	}
	var occs []map[string]json.RawMessage
	if err := json.Unmarshal(sc["occurrences"], &occs); len(sc["occurrences"]) != 0 && err != nil {
		return false, err
	}
	for _, o := range occs {
		for _, key := range []string{"actual_done_at", "actual_source", "roles"} {
			if _, ok := o[key]; ok {
				hasV2 = true
			}
		}
	}
	return hasV2, nil
}
