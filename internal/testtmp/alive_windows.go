//go:build windows

package testtmp

import "os"

// processAlive cannot ask cheaply here, so every owner counts as alive and
// only the legacy age rule removes anything.
func processAlive(int) bool { return true }

// ownedByMe: a Windows temp dir is per user already.
func ownedByMe(os.FileInfo) bool { return true }
