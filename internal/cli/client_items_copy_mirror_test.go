package cli_test

// The copy RESPONSE types in client_items_copy.go are hand-written mirrors
// of internal/server's, kept separate by the layering choice documented
// there. Nothing in the compiler stops them drifting. This test walks both
// shapes and fails on any difference in JSON field names, omitempty, field
// sets, or kinds — so a server-side rename is a red build here rather than a
// field that silently stops rendering in `pad item copy`.
//
// SCOPE, precisely: the two RESPONSE types, ItemCopyPreflight and
// ItemCopyResult, recursively. The REQUEST type is deliberately not here —
// the server's (itemCopyPreflightRequest) is unexported, so no test outside
// package server can name it. What covers the request instead is
// TestCopyItem_RequestShapeIsTheDocumentedOne in client_items_copy_test.go,
// which asserts the exact key set that goes out on the wire, and
// TestCopyPreflightAndCopySendIdenticalBodies, which pins the server's
// "both endpoints take the same body" contract. A request-side rename on the
// server is therefore caught by those two, not by this file.
//
// This file lives in the EXTERNAL test package (cli_test) because it imports
// internal/server, and package cli itself deliberately does not.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/server"
)

func TestItemCopyMirrorsMatchServerShapes(t *testing.T) {
	cases := []struct {
		name   string
		mirror any
		origin any
	}{
		{"ItemCopyPreflight", cli.ItemCopyPreflight{}, server.ItemCopyPreflight{}},
		{"ItemCopyResult", cli.ItemCopyResult{}, server.ItemCopyResult{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var findings []string
			compareJSONShape(reflect.TypeOf(tc.mirror), reflect.TypeOf(tc.origin), tc.name, &findings, map[string]bool{})
			for _, f := range findings {
				t.Errorf("%s", f)
			}
		})
	}
}

// compareJSONShape recurses over two struct types, comparing the JSON
// contract they express. mirror is the internal/cli copy; origin is
// internal/server's authority.
func compareJSONShape(mirror, origin reflect.Type, path string, findings *[]string, seen map[string]bool) {
	mirror = deref(mirror)
	origin = deref(origin)

	if mirror == origin {
		// Same type on both sides (e.g. *models.Item) — nothing to drift.
		return
	}

	key := path + "|" + mirror.String() + "|" + origin.String()
	if seen[key] {
		return
	}
	seen[key] = true

	if mirror.Kind() != origin.Kind() {
		*findings = append(*findings, fmt.Sprintf("%s: kind %s (cli) != %s (server)", path, mirror.Kind(), origin.Kind()))
		return
	}

	switch mirror.Kind() {
	case reflect.Struct:
		mf := jsonFields(mirror)
		of := jsonFields(origin)
		for _, name := range sortedUnion(mf, of) {
			m, inMirror := mf[name]
			o, inOrigin := of[name]
			switch {
			case !inMirror:
				*findings = append(*findings, fmt.Sprintf("%s.%s: present on the server type, MISSING from the cli mirror", path, name))
			case !inOrigin:
				*findings = append(*findings, fmt.Sprintf("%s.%s: present on the cli mirror, MISSING from the server type", path, name))
			default:
				if m.omitempty != o.omitempty {
					*findings = append(*findings, fmt.Sprintf("%s.%s: omitempty %v (cli) != %v (server)", path, name, m.omitempty, o.omitempty))
				}
				compareJSONShape(m.typ, o.typ, path+"."+name, findings, seen)
			}
		}
	case reflect.Slice, reflect.Array, reflect.Map:
		compareJSONShape(mirror.Elem(), origin.Elem(), path+"[]", findings, seen)
	default:
		if mirror.Kind() != origin.Kind() {
			*findings = append(*findings, fmt.Sprintf("%s: %s (cli) != %s (server)", path, mirror, origin))
		}
	}
}

type jsonField struct {
	typ       reflect.Type
	omitempty bool
}

func jsonFields(t reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: not on the wire
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "" {
			name = f.Name
		}
		omit := false
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omit = true
			}
		}
		out[name] = jsonField{typ: f.Type, omitempty: omit}
	}
	return out
}

func sortedUnion(a, b map[string]jsonField) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}
