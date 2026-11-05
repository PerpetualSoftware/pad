package models

import "testing"

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
		"end is builtin": {`{"date_field":"due_date","end_field":"created"}`, "end_field", CalendarCodeUnknownField},
		"end completed":  {`{"date_field":"due_date","end_field":"completed"}`, "end_field", CalendarCodeUnknownField},
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
