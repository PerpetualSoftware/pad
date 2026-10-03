// Package funcfield calls a function-typed field whose target the walk cannot
// see: a caller could have stored (*store.Store).DeleteItem in it.
package funcfield

import "github.com/PerpetualSoftware/pad/internal/store"

var _ *store.Store

type Writer struct{ Run func() error }

func Bad(w Writer) error { return w.Run() }
