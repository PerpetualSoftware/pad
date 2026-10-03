// Package helper writes in init.
package helper

import "github.com/PerpetualSoftware/pad/internal/store"

var S *store.Store

func init() {
	if S != nil {
		_ = S.DeleteItem("x")
	}
}
