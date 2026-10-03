// Package embeddedstore calls a human mutation promoted through an embedded
// *store.Store.
package embeddedstore

import "github.com/PerpetualSoftware/pad/internal/store"

type W struct{ *store.Store }

func Bad(w W) error { return w.DeleteItem("x") }
