package models

import "encoding/json"

// SchemaWithoutReservedFields returns schema with any reserved-metadata
// FieldDef removed — the schema as a consumer that reasons about an ITEM's
// field values must see it (BUG-2685).
//
// WHY A GRANDFATHERED DECLARATION IS IGNORED RATHER THAN REFUSED.
// `validateNoReservedFieldKeys` (BUG-2674) stops a schema NEWLY declaring one
// of these keys but deliberately grandfathers an existing declaration, so the
// shape survives wherever it predates that gate. Such a declaration is wrong in
// BOTH directions at once, which is what rules out refusing the write:
//
//   - it REFUSES the system's own value — `pad item note` / `pad item decide`
//     append an entry and PATCH the whole fields blob back, and a legacy
//     `implementation_notes: text` declaration fails that ARRAY on
//     validateFieldType, so the collection's items cannot be annotated at all;
//   - it ACCEPTS a caller's junk — the same declaration waves a bare string
//     through into the key, where the append helpers then refuse to touch it
//     (BUG-2627 part 3).
//
// Refusing would also break every system writer on such a collection, and a
// required legacy declaration already refuses ordinary creates over a field no
// client surface offers. Ignoring it restores the invariant these keys were
// always supposed to have: they are system-owned, and no collection schema
// speaks for them.
//
// Returns schema unchanged (no copy) when it declares no reserved key, which is
// every collection that has not been grandfathered — so the returned Fields
// slice may ALIAS the caller's. The contract is therefore read-only: treat the
// result as immutable.
func SchemaWithoutReservedFields(schema CollectionSchema) CollectionSchema {
	var reserved bool
	for _, f := range schema.Fields {
		if IsReservedItemField(f.Key) {
			reserved = true
			break
		}
	}
	if !reserved {
		return schema
	}

	out := schema
	out.Fields = make([]FieldDef, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if IsReservedItemField(f.Key) {
			continue
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}

// UnmarshalItemFieldSchema decodes a collection's stored schema JSON for a
// consumer that reasons about an ITEM's field values, applying
// SchemaWithoutReservedFields to the result.
//
// IT IS THE ONE PREDICATE, and it sits at the DECODE rather than at each use on
// purpose. BUG-2685's instrument counted 76 sites that consult a
// `CollectionSchema.Fields` and only 38 that decode one: a rule applied at the
// consultations is 76 patches that every future consumer has to remember, which
// is the shape of the last four review rounds this bug was filed out of. Every
// consultation inherits whatever the decode produced, so the decode is where
// one rule covers all of them.
//
// Signature mirrors json.Unmarshal's deliberately — same argument order, same
// error semantics — so converting a call site is a one-token edit and the
// remaining raw decodes stay greppable. TestReservedFieldDecodeSitesAreClassified
// enforces that: a NEW raw decode into a CollectionSchema fails the build unless
// it is added to that test's allow-list of collection-definition sites, with a
// reason.
//
// NOT for the sites where the collection DEFINITION is the subject — schema
// create/update input, the grandfather test's own `prevSchema`, and the CLI /
// MCP paths that render or accept a schema. There the declaration is the thing
// being talked about, and stripping it would be wrong rather than merely
// unnecessary: feeding a stripped `prevSchema` to validateNoReservedFieldKeys
// would reclassify every existing declaration as newly introduced and refuse
// every update to such a collection.
func UnmarshalItemFieldSchema(data []byte, dst *CollectionSchema) error {
	var schema CollectionSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		return err
	}
	*dst = SchemaWithoutReservedFields(schema)
	return nil
}
