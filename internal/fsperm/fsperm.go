// Package fsperm holds the open flags used to change a file or directory mode
// through a descriptor rather than by path.
//
// Opening without following a symlink and then calling Chmod on the opened
// file means a symlink swapped in at the path after a check cannot redirect
// the mode change to another file.
package fsperm
