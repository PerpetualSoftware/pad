package server

import (
	"encoding/json"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// closeStateChangeCode is the move doors' refusal when a move would change an
// item's open/done/abandoned state without the caller saying so (BUG-2367
// item 4). The copy answers the same sentence under its validation_error, and
// the preflight turns it into a needs_value row with reason `state_change`.
const closeStateChangeCode = "state_change_requires_value"

// collectionSettingsOf parses a collection's settings, treating unreadable
// settings as empty the way every other done-field caller does.
func collectionSettingsOf(c *models.Collection) models.CollectionSettings {
	var settings models.CollectionSettings
	if c != nil && c.Settings != "" {
		_ = json.Unmarshal([]byte(c.Settings), &settings)
	}
	return settings
}
