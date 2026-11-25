package models

import (
	"encoding/json"
	"fmt"
	"io"
)

// EncodeWorkspaceExport writes x as compact JSON to w, streaming item_versions
// from versions so version rows do not have to be held in memory at once
// (TASK-4839).
//
// It marshals a copy of x with ItemVersions cleared (omitting "item_versions"
// via omitempty) and splices any yielded versions onto the end of the JSON
// object. When versions is nil or yields no elements, "item_versions" is
// omitted.
func EncodeWorkspaceExport(w io.Writer, x *WorkspaceExport, versions func(yield func(*ItemVersionExport) error) error) error {
	if x == nil {
		_, err := io.WriteString(w, "null")
		return err
	}

	baseVal := *x
	baseVal.ItemVersions = nil
	base, err := json.Marshal(&baseVal)
	if err != nil {
		return err
	}
	if len(base) < 2 || base[0] != '{' || base[len(base)-1] != '}' {
		return fmt.Errorf("encode workspace export: unexpected base JSON object")
	}

	if versions == nil {
		_, err = w.Write(base)
		return err
	}

	if _, err := w.Write(base[:len(base)-1]); err != nil {
		return err
	}

	emptyObj := len(base) == 2
	wroteAny := false
	err = versions(func(v *ItemVersionExport) error {
		elem, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if !wroteAny {
			prefix := `,"item_versions":[`
			if emptyObj {
				prefix = `"item_versions":[`
			}
			if _, err := io.WriteString(w, prefix); err != nil {
				return err
			}
			wroteAny = true
		} else if _, err := io.WriteString(w, ","); err != nil {
			return err
		}
		_, err = w.Write(elem)
		return err
	})
	if err != nil {
		return err
	}

	if wroteAny {
		if _, err := io.WriteString(w, "]"); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "}")
	return err
}
