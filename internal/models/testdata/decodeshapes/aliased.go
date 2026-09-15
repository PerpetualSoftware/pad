package decodeshapes

import (
	jsonx "encoding/json"

	m "github.com/PerpetualSoftware/pad/internal/models"
)

// 13. the models package under a LOCAL ALIAS.
func s13(b []byte) {
	var aliasedPkgDest m.CollectionSchema
	_ = jsonx.Unmarshal(b, &aliasedPkgDest)
}

// 14. a schema shape whose element type is declared INLINE.
type inlineShaped struct {
	Fields []struct {
		Key  string `json:"key"`
		Type string `json:"type"`
	} `json:"fields"`
}

// 18. a decode into a schema-shaped type whose ELEMENT type is declared inline
// and which is not models.CollectionSchema at all — the bootstrapSchema case,
// generalised. A guard keyed on the canonical type cannot see this one.
func s18(b []byte) {
	var inlineDest inlineShaped
	_ = jsonx.Unmarshal(b, &inlineDest)
}
