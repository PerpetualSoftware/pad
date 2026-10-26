//go:build windows

package testtmp

import (
	"fmt"
	"os"
)

// processAlive cannot ask cheaply here, so every owner counts as alive and
// a Windows sweep removes nothing.
func processAlive(int) bool { return true }

// ownedByMe is never consulted for removal here: processAlive keeps
// everything.
func ownedByMe(os.FileInfo) bool { return true }

// mkdirOwned takes no lock here: ownerState reports every directory as
// lockless, and processAlive then keeps it.
func mkdirOwned(dir, prefix string) (string, *os.File, error) {
	path, err := os.MkdirTemp(dir, fmt.Sprintf("%s%d-*", prefix, os.Getpid()))
	return path, nil, err
}

func ownerState(string) (ownership, func()) { return ownerNoLock, nil }
