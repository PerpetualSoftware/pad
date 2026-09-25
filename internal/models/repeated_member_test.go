package models

import (
	"encoding/json"
	"errors"
	"testing"
)

// BUG-3219. RefuseRepeatedMember must agree with encoding/json about which
// keys land in one struct field: a repeat it misses is a silent drop, and a
// repeat it invents is a refusal of a valid request.
func TestRefuseRepeatedMember(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"absent", `{"title":"x"}`, false},
		{"once", `{"fields_patch":{"a":1}}`, false},
		{"exact repeat", `{"fields_patch":{"a":1},"fields_patch":{"b":2}}`, true},
		{"repeat with members between", `{"fields_patch":{"a":1},"title":"x","fields_patch":{"b":2}}`, true},
		{"case-variant repeat", `{"fields_patch":{"a":1},"Fields_Patch":{"b":2}}`, true},
		{"two case variants, no exact spelling", `{"FIELDS_PATCH":{"a":1},"Fields_patch":{"b":2}}`, true},
		{"null then object", `{"fields_patch":null,"fields_patch":{"b":2}}`, true},
		// Nested keys are not the request's members: a field literally named
		// fields_patch inside the patch is a field.
		{"nested name is not a member", `{"fields_patch":{"fields_patch":1},"fields":{"fields_patch":2}}`, false},
		// A different member that merely contains the name.
		{"prefix is not the member", `{"fields_patch":{"a":1},"fields_patch_x":{"b":2}}`, false},
		// Folding is encoding/json's: U+212A KELVIN SIGN folds to K, so it
		// matches a `k` field there, and must match here.
		{"unicode fold (Kelvin sign)", `{"k":1,"` + "K" + `":2}`, true},
		{"not an object", `[1,2]`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			member := "fields_patch"
			if c.name == "unicode fold (Kelvin sign)" {
				member = "k"
			}
			err := RefuseRepeatedMember([]byte(c.body), member)
			var rep *RepeatedMemberError
			if got := errors.As(err, &rep); got != c.want {
				t.Fatalf("repeat detected = %v, want %v (err %v)", got, c.want, err)
			}
			if c.want && rep.Member != member {
				t.Fatalf("names %q, want %q", rep.Member, member)
			}
		})
	}
}

// The detector's notion of "same field" is checked against encoding/json
// itself, not against a restatement of its rule: for each spelling, decoding
// into a struct shows whether the key reaches the field.
func TestRefuseRepeatedMemberAgreesWithEncodingJSON(t *testing.T) {
	for _, key := range []string{"k", "K", "K", "kk", "k ", "ｋ"} {
		var s struct {
			K json.RawMessage `json:"k"`
		}
		if err := json.Unmarshal([]byte(`{"`+key+`":1}`), &s); err != nil {
			t.Fatal(err)
		}
		reaches := s.K != nil
		detected := RefuseRepeatedMember([]byte(`{"k":0,"`+key+`":1}`), "k") != nil
		if reaches != detected {
			t.Errorf("key %q: encoding/json decodes it into k = %v, detector calls it a repeat = %v", key, reaches, detected)
		}
	}
}

// The FieldValues backstop: a struct that forgets RefuseRepeatedMember still
// cannot silently keep only the last occurrence.
func TestFieldValuesRefusesSecondDecode(t *testing.T) {
	var s struct {
		F FieldValues `json:"f"`
	}
	err := json.Unmarshal([]byte(`{"f":{"a":1},"f":{"b":2}}`), &s)
	var rep *RepeatedMemberError
	if !errors.As(err, &rep) {
		t.Fatalf("want RepeatedMemberError, got %v (decoded %v)", err, s.F)
	}
	s.F = nil
	if err := json.Unmarshal([]byte(`{"f":{"a":1}}`), &s); err != nil || s.F["a"] == nil {
		t.Fatalf("one member: err %v, decoded %v", err, s.F)
	}
}

// ItemUpdate refuses at its own decode, so every caller of it inherits this.
func TestItemUpdateRefusesRepeatedFieldsPatch(t *testing.T) {
	var u ItemUpdate
	err := json.Unmarshal([]byte(`{"fields_patch":{"a":1},"fields_patch":{"b":2}}`), &u)
	var rep *RepeatedMemberError
	if !errors.As(err, &rep) || rep.Member != "fields_patch" {
		t.Fatalf("want RepeatedMemberError naming fields_patch, got %v", err)
	}
	u = ItemUpdate{}
	if err := json.Unmarshal([]byte(`{"fields_patch":{"a":1}}`), &u); err != nil || u.FieldsPatch["a"] == nil {
		t.Fatalf("one member: err %v, patch %v", err, u.FieldsPatch)
	}
}
