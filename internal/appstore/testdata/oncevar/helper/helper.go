// Package helper exposes a function value that writes.
package helper

import "github.com/PerpetualSoftware/pad/internal/store"

var S *store.Store

var Remove = func() { _ = S.DeleteItem("x") }
