// Package helperinit imports a helper whose init writes.
package helperinit

import (
	"github.com/PerpetualSoftware/pad/internal/appstore/testdata/helperinit/helper"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func Bad(s *store.Store) { helper.S = s }
