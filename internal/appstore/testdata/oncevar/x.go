// Package oncevar hands a package-level function value to code outside the
// walk, which calls it.
package oncevar

import (
	"sync"

	"github.com/PerpetualSoftware/pad/internal/appstore/testdata/oncevar/helper"
	"github.com/PerpetualSoftware/pad/internal/store"
)

var _ *store.Store

func Bad() { new(sync.Once).Do(helper.Remove) }
