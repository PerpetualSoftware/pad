package cli

import "time"

var displayLocationResolver func() *time.Location

// SetDisplayLocationResolver installs the CLI process's lazy viewer-zone lookup.
// A nil resolver restores the UTC default, including for non-CLI consumers.
func SetDisplayLocationResolver(resolve func() *time.Location) {
	displayLocationResolver = resolve
}

// DisplayLocation returns the viewer's zone, never the machine's local zone.
func DisplayLocation() *time.Location {
	if displayLocationResolver != nil {
		if loc := displayLocationResolver(); loc != nil {
			return loc
		}
	}
	return time.UTC
}

// FormatInstant renders an instant for the viewer. JSON timestamps bypass it.
func FormatInstant(t time.Time, layout string) string {
	return t.In(DisplayLocation()).Format(layout)
}
