package models

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The calendar view (TASK-4239): a saved view of view_type "calendar" carries
// the date field its items are placed by, and optionally the field that ends a
// span. The route that reads a calendar takes the same two names as query
// parameters and runs the same check.

// CalendarViewType is the view_type whose config is validated.
const CalendarViewType = "calendar"

// CalendarBuiltinDateFields are the item dates that are not schema fields:
// the instant the item was created and the instant it last moved into a
// positive terminal status. They place an item (date_field) but cannot end a
// span (end_field).
var CalendarBuiltinDateFields = []string{"created", "completed"}

// Codes of CalendarConfigError.
const (
	CalendarCodeRequired     = "required"
	CalendarCodeUnknownField = "unknown_field"
	CalendarCodeNotDate      = "not_date"
	CalendarCodeConfig       = "config"
)

// CalendarConfigError is a refusal naming the offending field.
type CalendarConfigError struct {
	Field   string // date_field | end_field | config
	Code    string
	Message string
}

func (e *CalendarConfigError) Error() string { return e.Message }

// IsCalendarBuiltinDateField reports whether key is `created` or `completed`.
func IsCalendarBuiltinDateField(key string) bool {
	for _, b := range CalendarBuiltinDateFields {
		if key == b {
			return true
		}
	}
	return false
}

// ValidateCalendarFields checks a calendar's date_field and end_field against
// the collection's schema. date_field is required: a schema date field or a
// built-in. end_field is optional: a schema date field only.
func ValidateCalendarFields(dateField, endField string, schema CollectionSchema) *CalendarConfigError {
	dateType := func(key string) (found bool, typ string) {
		for _, f := range schema.Fields {
			if f.Key == key {
				return true, f.Type
			}
		}
		return false, ""
	}
	if dateField == "" {
		return &CalendarConfigError{Field: "date_field", Code: CalendarCodeRequired, Message: "date_field is required for a calendar: a date field of the collection, or created or completed"}
	}
	if !IsCalendarBuiltinDateField(dateField) {
		found, typ := dateType(dateField)
		switch {
		case !found:
			return &CalendarConfigError{Field: "date_field", Code: CalendarCodeUnknownField, Message: fmt.Sprintf("date_field %q is not a field of this collection", dateField)}
		case typ != "date":
			return &CalendarConfigError{Field: "date_field", Code: CalendarCodeNotDate, Message: fmt.Sprintf("date_field %q is not a date field", dateField)}
		}
	}
	if endField != "" {
		found, typ := dateType(endField)
		switch {
		case !found:
			return &CalendarConfigError{Field: "end_field", Code: CalendarCodeUnknownField, Message: fmt.Sprintf("end_field %q is not a field of this collection", endField)}
		case typ != "date":
			return &CalendarConfigError{Field: "end_field", Code: CalendarCodeNotDate, Message: fmt.Sprintf("end_field %q is not a date field", endField)}
		}
	}
	return nil
}

// CalendarViewConfig reads date_field and end_field out of a view config. A
// config that is not a JSON object, or whose two keys are not strings, is
// refused.
func CalendarViewConfig(config string) (dateField, endField string, err *CalendarConfigError) {
	bad := func(msg string) (string, string, *CalendarConfigError) {
		return "", "", &CalendarConfigError{Field: "config", Code: CalendarCodeConfig, Message: msg}
	}
	if strings.TrimSpace(config) == "" {
		config = "{}"
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(config), &doc) != nil || doc == nil {
		return bad("config of a calendar view must be a JSON object")
	}
	for _, k := range []string{"date_field", "end_field"} {
		raw, ok := doc[k]
		if !ok || string(raw) == "null" {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", "", &CalendarConfigError{Field: k, Code: CalendarCodeConfig, Message: k + " must be a string"}
		}
		if k == "date_field" {
			dateField = v
		} else {
			endField = v
		}
	}
	return dateField, endField, nil
}

// ValidateCalendarViewConfig is the whole check of a calendar view's config.
func ValidateCalendarViewConfig(config string, schema CollectionSchema) *CalendarConfigError {
	dateField, endField, err := CalendarViewConfig(config)
	if err != nil {
		return err
	}
	return ValidateCalendarFields(dateField, endField, schema)
}
