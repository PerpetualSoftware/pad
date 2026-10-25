package config

// resetRelativeDataDirWarning lets each test see the once-per-process warning
// afresh, with the default (stderr) sink.
func resetRelativeDataDirWarning() {
	relativeDataDirMu.Lock()
	defer relativeDataDirMu.Unlock()
	relativeDataDirWarned = false
	relativeDataDirSink = nil
}
