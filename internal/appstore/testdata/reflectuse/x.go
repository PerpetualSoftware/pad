// Package reflectuse reaches a method by reflection: rule reflect-unsafe.
package reflectuse

import (
	"reflect"

	"github.com/PerpetualSoftware/pad/internal/store"
)

func Bad(s *store.Store) {
	reflect.ValueOf(s).MethodByName("CreateItem")
}
