package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4051: after a partial sent record, the dispatcher's failure path parks
// only the suffix whose UPDATE did not land. Kills: M3b.
func TestPushDispatch_RecordSentErrorParksOnlyUnlandedIDs(t *testing.T) {
	srv := &Server{}
	ids := []string{"landed-a", "landed-b", "unlanded-c", "unlanded-d"}
	err := &store.RecordSentError{Landed: 2}
	srv.parkUnrecorded(ids[recordedPrefix(err):], nil, time.Now())
	parked, _ := srv.unrecordedSnapshot()
	if len(parked) != 2 || parked[ids[0]] || parked[ids[1]] || !parked[ids[2]] || !parked[ids[3]] {
		t.Fatalf("parked = %v, want only %v", parked, ids[2:])
	}
}
