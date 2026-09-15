// Package decodeshapes is a FIXTURE, not compiled code — it lives under
// testdata so the go tool ignores it, and exists to be parsed by
// TestGuardCatchesEveryDestinationShape.
//
// It is the population table for BUG-2685's guard. Three consecutive review
// rounds each named another destination spelling the guard did not handle, one
// at a time; CONVE-35 says a loop that keeps finding new members of a class is
// measuring the enumeration rather than the code. So the class is written down
// HERE, once, and the guard is tested against the whole of it.
//
// Adding a spelling to this file is how you extend that table. A spelling the
// guard cannot see fails the test, which is the point.
package decodeshapes

import (
	jsonx "encoding/json"
	"io"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type holder struct {
	fieldDest models.CollectionSchema
}

func makeDest() *models.CollectionSchema { return &models.CollectionSchema{} }

// 1. address of a local value
func s1(b []byte) {
	var localDest models.CollectionSchema
	_ = jsonx.Unmarshal(b, &localDest)
}

// 2. a parameter that is already a pointer, passed bare
func s2(b []byte, paramDest *models.CollectionSchema) {
	_ = jsonx.Unmarshal(b, paramDest)
}

// 3. address of a struct field whose struct is declared elsewhere in the package
func s3(b []byte) {
	var h holder
	_ = jsonx.Unmarshal(b, &h.fieldDest)
}

// 4. new(T)
func s4(b []byte) {
	_ = jsonx.Unmarshal(b, new(models.CollectionSchema))
}

// 5. address of a composite literal
func s5(b []byte) {
	_ = jsonx.Unmarshal(b, &models.CollectionSchema{})
}

// 6. address of an element of a container of schemas
func s6(b []byte) {
	sliceDest := make([]models.CollectionSchema, 1)
	_ = jsonx.Unmarshal(b, &sliceDest[0])
}

// 7. the result of a helper that returns a schema pointer
func s7(b []byte) {
	_ = jsonx.Unmarshal(b, makeDest())
}

// 8. a json.Decoder, through an ALIASED import of encoding/json
func s8(r io.Reader) {
	var decoderDest models.CollectionSchema
	_ = jsonx.NewDecoder(r).Decode(&decoderDest)
}

// 9. a short declaration with an inferred type
func s9(b []byte) {
	shortDest := models.CollectionSchema{}
	_ = jsonx.Unmarshal(b, &shortDest)
}

// 10. a var declaration with an inferred type
func s10(b []byte) {
	var inferredDest = models.CollectionSchema{}
	_ = jsonx.Unmarshal(b, &inferredDest)
}

// 11. a var whose inferred type comes from new(T)
func s11(b []byte) {
	var newDest = new(models.CollectionSchema)
	_ = jsonx.Unmarshal(b, newDest)
}

// 12. a pointer taken from a schema already named
func s12(b []byte) {
	var base models.CollectionSchema
	aliasDest := &base
	_ = jsonx.Unmarshal(b, aliasDest)
}

// 13. models imported under an ALIAS (see aliased.go in this directory)

// NOT a destination: the SOURCE argument mentioning a schema must not match, or
// every correctly-converted site in the tree reports itself as raw.
func notADestination(c models.Collection) {
	var out models.CollectionSchema
	_ = models.UnmarshalItemFieldSchema([]byte(c.Schema), &out)
}
