package models

import (
	"fmt"
	"strings"
)

// CloseState is where an item stands on its collection's done field: still
// open, closed having delivered, or closed without delivering. It is derived
// from IsTerminalItem and IsAbandonedItem and never decided separately, so it
// cannot disagree with the changelog, standup or progress about one value.
type CloseState string

const (
	CloseStateOpen      CloseState = "open"
	CloseStateDone      CloseState = "done"
	CloseStateAbandoned CloseState = "abandoned"
)

// ItemCloseState classifies an item's fields against its collection.
func ItemCloseState(fields map[string]any, schema CollectionSchema, settings CollectionSettings) CloseState {
	switch {
	case !IsTerminalItem(fields, schema, settings):
		return CloseStateOpen
	case IsAbandonedItem(fields, schema, settings):
		return CloseStateAbandoned
	default:
		return CloseStateDone
	}
}

// doneSelectField returns the done field's definition when the schema really
// declares it as a select. TerminalValuesForDoneField falls back to a default
// vocabulary for any schema at all, so "has a terminal list" is not the same
// question as "has a state a caller could set".
func doneSelectField(schema CollectionSchema, settings CollectionSettings) (FieldDef, bool) {
	key := DoneFieldKey(schema, settings)
	for _, f := range schema.Fields {
		if f.Key == key && f.Type == "select" {
			return f, true
		}
	}
	return FieldDef{}, false
}

// CloseStateChange describes a move or copy that would change an item's
// CloseState without the caller having said so (BUG-2367 item 4).
type CloseStateChange struct {
	// Key and Label name the DESTINATION done field; Options are its values,
	// which is what a caller may set it to.
	Key, Label string
	Options    []string
	From, To   CloseState
	// FromValue / ToValue are the done-field values on each side; empty when
	// that side had none.
	FromValue, ToValue string
}

// MigrateCloseStateChange reports whether moving an item from the source
// collection to the destination would change its CloseState, given the
// destination fields as they will be written (after overrides and injected
// defaults). It returns nil when there is no change, when the caller supplied
// the destination done field (supplied(key) true — an explicit value is the
// override the lead ruling names), or when the destination declares no done
// select field, so there is no state to change and no value a caller could
// set.
//
// Only the STATE is compared, never the value (lead ruling, day 78): `done` in
// one collection and `shipped` in another are both closed-delivered, and carry
// without asking.
func MigrateCloseStateChange(
	srcFields map[string]any, srcSchema CollectionSchema, srcSettings CollectionSettings,
	dstFields map[string]any, dstSchema CollectionSchema, dstSettings CollectionSettings,
	supplied func(key string) bool,
) *CloseStateChange {
	def, ok := doneSelectField(dstSchema, dstSettings)
	if !ok {
		return nil
	}
	if supplied != nil && supplied(def.Key) {
		return nil
	}
	from := ItemCloseState(srcFields, srcSchema, srcSettings)
	to := ItemCloseState(dstFields, dstSchema, dstSettings)
	if from == to {
		return nil
	}
	srcKey := DoneFieldKey(srcSchema, srcSettings)
	fromValue, _ := srcFields[srcKey].(string)
	toValue, _ := dstFields[def.Key].(string)
	label := def.Label
	if label == "" {
		label = def.Key
	}
	return &CloseStateChange{
		Key: def.Key, Label: label, Options: def.Options,
		From: from, To: to, FromValue: fromValue, ToValue: toValue,
	}
}

// Message is the one sentence every door refuses with. It names the change,
// the field and the values a caller may choose from, which is everything an
// agent needs to retry and everything a person needs to decide.
func (c *CloseStateChange) Message() string {
	side := func(state CloseState, value string) string {
		if value == "" {
			return string(state)
		}
		return fmt.Sprintf("%s (%s %q)", state, c.Key, value)
	}
	return fmt.Sprintf("this would change the item from %s to %s; set %s explicitly to one of: %s",
		side(c.From, c.FromValue), side(c.To, c.ToValue), c.Key, strings.Join(c.Options, ", "))
}

// Details is the structured half of the refusal: the field to set and the
// values it takes, so a client builds its retry from data rather than from the
// sentence.
func (c *CloseStateChange) Details() map[string]any {
	return map[string]any{"field": c.Key, "options": c.Options, "from": string(c.From), "to": string(c.To)}
}
