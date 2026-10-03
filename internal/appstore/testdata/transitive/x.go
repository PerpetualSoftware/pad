// Package transitive reaches a human mutation through a helper in another
// package: the walk must follow it.
package transitive

import (
	"github.com/PerpetualSoftware/pad/internal/appstore/testdata/transitive/helper"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func Bad(s *store.Store) error { return helper.Do(s) }
