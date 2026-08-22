package server

// Test-only shims for the key builders, which became METHODS in BUG-2724
// because a key name now depends on the registry's namespace.
//
// They delegate to a zero-valued registry rather than rebuilding the
// names, so the historical-name expectations in these tests keep being
// checked against the production construction. Duplicating the format
// string here is what would let the two drift — which is the whole defect
// class the namespace change was structured to avoid.
var defaultKeyNamer = &RedisSessionPresence{}

func sessionKey(userID, sessionID string) string {
	return defaultKeyNamer.sessionKey(userID, sessionID)
}
func sessionIndexKey(userID string) string { return defaultKeyNamer.sessionIndexKey(userID) }
func userIDKeyPrefix(userID string) string { return defaultKeyNamer.userIDKeyPrefix(userID) }
