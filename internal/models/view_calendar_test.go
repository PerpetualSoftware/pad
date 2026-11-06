package models

import (
	"strings"
	"testing"
)

func TestValidateCalendarViewConfig(t *testing.T) {
	schema := CollectionSchema{Fields: []FieldDef{
		{Key: "status", Type: "select"}, {Key: "due_date", Type: "date"}, {Key: "end_date", Type: "date"},
	}}
	for name, c := range map[string]struct{ config, field, code string }{
		"empty":          {``, "date_field", CalendarCodeRequired},
		"object":         {`{}`, "date_field", CalendarCodeRequired},
		"array":          {`[]`, "config", CalendarCodeConfig},
		"null":           {`null`, "config", CalendarCodeConfig},
		"string":         {`"x"`, "config", CalendarCodeConfig},
		"number field":   {`{"date_field":1}`, "date_field", CalendarCodeConfig},
		"unknown":        {`{"date_field":"x"}`, "date_field", CalendarCodeUnknownField},
		"not a date":     {`{"date_field":"status"}`, "date_field", CalendarCodeNotDate},
		"unknown end":    {`{"date_field":"due_date","end_field":"x"}`, "end_field", CalendarCodeUnknownField},
		"end not date":   {`{"date_field":"due_date","end_field":"status"}`, "end_field", CalendarCodeNotDate},
		"end is builtin": {`{"date_field":"due_date","end_field":"created"}`, "end_field", CalendarCodeBuiltinEnd},
		"end completed":  {`{"date_field":"due_date","end_field":"completed"}`, "end_field", CalendarCodeBuiltinEnd},
	} {
		err := ValidateCalendarViewConfig(c.config, schema)
		if err == nil || err.Field != c.field || err.Code != c.code {
			t.Errorf("%s: %+v, want %s/%s", name, err, c.field, c.code)
		}
	}
	for _, ok := range []string{`{"date_field":"due_date"}`, `{"date_field":"created"}`, `{"date_field":"completed"}`,
		`{"date_field":"due_date","end_field":"end_date"}`, `{"date_field":"due_date","end_field":null}`, `{"date_field":"due_date","end_field":""}`} {
		if err := ValidateCalendarViewConfig(ok, schema); err != nil {
			t.Errorf("%s refused: %+v", ok, err)
		}
	}
}

func TestValidateCalendarFieldsRefusesAKeyTheStoreCannotRead(t *testing.T) {
	schema := CollectionSchema{Fields: []FieldDef{
		{Key: "start date", Type: "date"}, {Key: "due_date", Type: "date"}, {Key: "état", Type: "date"},
	}}
	for name, c := range map[string]struct{ date, end, field string }{
		"date_field space":   {"start date", "", "date_field"},
		"end_field space":    {"due_date", "start date", "end_field"},
		"date_field unicode": {"état", "", "date_field"},
	} {
		err := ValidateCalendarFields(c.date, c.end, schema)
		if err == nil || err.Field != c.field || err.Code != CalendarCodeUnsupportedKey {
			t.Errorf("%s: %+v, want %s/%s", name, err, c.field, CalendarCodeUnsupportedKey)
		}
	}
	if err := ValidateCalendarFields("due_date", "", schema); err != nil {
		t.Errorf("plain key refused: %+v", err)
	}
}

func TestValidateCalendarFieldsRefusesABuiltinEndFieldWhateverTheSchema(t *testing.T) {
	schema := CollectionSchema{Fields: []FieldDef{
		{Key: "due_date", Type: "date"}, {Key: "completed", Type: "date"}, {Key: "created", Type: "date"},
	}}
	for _, end := range []string{"created", "completed"} {
		err := ValidateCalendarFields("due_date", end, schema)
		if err == nil || err.Field != "end_field" || err.Code != CalendarCodeBuiltinEnd || !strings.Contains(err.Message, end) {
			t.Errorf("end_field %s with a same-named schema field: %+v", end, err)
		}
		if err := ValidateCalendarViewConfig(`{"date_field":"due_date","end_field":"`+end+`"}`, schema); err == nil || err.Code != CalendarCodeBuiltinEnd {
			t.Errorf("config end_field %s: %+v", end, err)
		}
	}
	// date_field keeps naming the built-in (it shadows a same-named field).
	for _, d := range []string{"created", "completed"} {
		if err := ValidateCalendarFields(d, "", schema); err != nil {
			t.Errorf("date_field %s refused: %+v", d, err)
		}
	}
}
