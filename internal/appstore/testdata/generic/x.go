// Package generic reaches a human mutation through an instantiated generic
// method.
package generic

import (
	"github.com/PerpetualSoftware/pad/internal/appstore/testdata/generic/helper"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func Bad(s *store.Store) error { return helper.Box[int]{}.Remove(s, "x") }
