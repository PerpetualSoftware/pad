package decodeshapes

import (
	jsonx "encoding/json"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// 15. a TYPE ALIAS for a schema.
type aliasType = models.CollectionSchema

func s15(b []byte) {
	var typeAliasDest aliasType
	_ = jsonx.Unmarshal(b, &typeAliasDest)
}

// 16. a variable holding the result of a helper that returns a schema pointer.
func s16(b []byte) {
	helperResultDest := makeDest()
	_ = jsonx.Unmarshal(b, helperResultDest)
}

// 17. a CLOSURE parameter.
func s17(b []byte) {
	f := func(closureDest *models.CollectionSchema) {
		_ = jsonx.Unmarshal(b, closureDest)
	}
	f(nil)
}

// 19. a NAMED slice type for the fields member — the shape test must unwrap it.
type fieldList []models.FieldDef

type namedSliceSchema struct {
	Fields fieldList `json:"fields"`
}

func s19(b []byte) {
	var namedSliceDest namedSliceSchema
	_ = jsonx.Unmarshal(b, &namedSliceDest)
}

// 20. a call through a FUNCTION VALUE holding json.Unmarshal.
func s20(b []byte) {
	decode := jsonx.Unmarshal
	var funcValueDest models.CollectionSchema
	_ = decode(b, &funcValueDest)
}

// 21. an INTERFACE destination, which hides the concrete type. Reported for
// classification rather than resolved.
func s21(b []byte) {
	var schema models.CollectionSchema
	var ifaceDest any = &schema
	_ = jsonx.Unmarshal(b, ifaceDest)
}

// NOT resolved, and deliberately: a POINTER to an interface. Including it would
// pull in every decode-arbitrary-JSON call in the tree (measured: 20 sites
// become 38). See the comment beside the interface arm in the guard.
func s22(b []byte) {
	var schema models.CollectionSchema
	var ptrIfaceDest any = &schema
	_ = jsonx.Unmarshal(b, &ptrIfaceDest)
}

// 23. a struct EMBEDDING a schema carries the same JSON members.
type embeddedSchema struct {
	models.CollectionSchema
	Extra string `json:"extra"`
}

func s23(b []byte) {
	var embeddedDest embeddedSchema
	_ = jsonx.Unmarshal(b, &embeddedDest)
}

// 24. a fields member whose ELEMENT is a pointer.
type ptrElemSchema struct {
	Fields []*models.FieldDef `json:"fields"`
}

func s24(b []byte) {
	var ptrElemDest ptrElemSchema
	_ = jsonx.Unmarshal(b, &ptrElemDest)
}

// 25. an UNTAGGED fields member. encoding/json matches it by name,
// case-insensitively, so this decodes the same JSON as the tagged form.
type untaggedSchema struct {
	Fields []models.FieldDef
}

func s25(b []byte) {
	var untaggedDest untaggedSchema
	_ = jsonx.Unmarshal(b, &untaggedDest)
}

// 26. a fields member EXCLUDED from JSON is not a schema member at all.
type excludedFieldsSchema struct {
	Fields []models.FieldDef `json:"-"`
	Real   string            `json:"real"`
}

func s26(b []byte) {
	var excludedDest excludedFieldsSchema
	_ = jsonx.Unmarshal(b, &excludedDest)
}
