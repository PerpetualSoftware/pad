//go:build windows

package testtmp

import "os"

// processAlive cannot ask cheaply here, so every owner counts as alive and
// a Windows sweep removes nothing.
func processAlive(int) bool { return true }

// ownedByMe is never consulted for removal here: processAlive keeps
// everything.
func ownedByMe(os.FileInfo) bool { return true }
