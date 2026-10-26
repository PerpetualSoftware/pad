//go:build !linux

package cli

import "errors"

// initPIDNamespace has no meaning off Linux; nothing compares against it
// because currentPIDNamespace never succeeds here.
const initPIDNamespace uint64 = 0

// namespacesSupported is false off Linux: there is no namespace to be
// across, so a failed read is not a possible boundary.
const namespacesSupported = false

var errNoNamespaces = errors.New("namespaces are a Linux concept")

// currentPIDNamespace and currentMountNamespace are unavailable off Linux.
// Records are then written without pid_ns or mnt_ns and judged by the
// probes as before (BUG-4090).
func currentPIDNamespace() (uint64, error)   { return 0, errNoNamespaces }
func currentMountNamespace() (uint64, error) { return 0, errNoNamespaces }
