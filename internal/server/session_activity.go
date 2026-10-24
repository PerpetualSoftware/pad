package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/redis/go-redis/v9"
)

const sessionActivityTTL = 30 * time.Minute

const reorderSlack = 7 * time.Second

// Two reports from one worker can only race while both are in flight, so their
// at values can be separated by at most the report bound plus spawn and
// scheduling slack. Any larger gap must be a clock step.
const reorderWindow = models.SessionActivityReportBound + reorderSlack

type sessionActivityKey struct{ userID, workerID string }
type sessionActivity struct {
	Busy     bool                      `json:"busy"`
	At       time.Time                 `json:"at"` // Server receipt, for expiry.
	ClientAt *sessionActivityTimestamp `json:"client_at,omitempty"`
}

// Separate seconds and nanoseconds remain exact in Lua's numeric type, unlike
// UnixNano. A nil timestamp represents an older client without ordering data.
type sessionActivityTimestamp struct {
	Seconds int64 `json:"seconds"`
	Nanos   int   `json:"nanos"`
}

func activityTimestamp(at *time.Time) *sessionActivityTimestamp {
	if at == nil {
		return nil
	}
	return &sessionActivityTimestamp{Seconds: at.Unix(), Nanos: at.Nanosecond()}
}

func (at *sessionActivityTimestamp) before(other *sessionActivityTimestamp) bool {
	return at.Seconds < other.Seconds || (at.Seconds == other.Seconds && at.Nanos < other.Nanos)
}

func presenceNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

func (p *MemorySessionPresence) SetActivity(userID, workerID string, busy bool, at time.Time, clientAt *time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := presenceNow(p.now)
	// Prune on writes too, so workers that never reconnect leave no permanent entry.
	for key, record := range p.activity {
		if !now.Before(record.At.Add(sessionActivityTTL)) {
			delete(p.activity, key)
		}
	}
	if p.activity == nil {
		p.activity = make(map[sessionActivityKey]sessionActivity)
	}
	key := sessionActivityKey{userID, workerID}
	timestamp := activityTimestamp(clientAt)
	if previous := p.activity[key].ClientAt; timestamp != nil && previous != nil && timestamp.before(previous) {
		previousAt := time.Unix(previous.Seconds, int64(previous.Nanos))
		if previousAt.Sub(*clientAt) <= reorderWindow {
			return nil
		}
	}
	if !now.Before(at.Add(sessionActivityTTL)) {
		delete(p.activity, key)
		return nil
	}
	p.activity[key] = sessionActivity{Busy: busy, At: at, ClientAt: timestamp}
	return nil
}

func (p *MemorySessionPresence) BusyWorkers(userID string, workerIDs []string) (map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	busy := make(map[string]bool, len(workerIDs))
	now := presenceNow(p.now)
	for _, workerID := range workerIDs {
		if workerID == "" {
			continue
		}
		if _, seen := busy[workerID]; seen {
			continue
		}
		busy[workerID] = false
		key := sessionActivityKey{userID, workerID}
		record, ok := p.activity[key]
		if !ok {
			continue
		}
		if !now.Before(record.At.Add(sessionActivityTTL)) {
			delete(p.activity, key)
			continue
		}
		busy[workerID] = record.Busy
	}
	return busy, nil
}

func (p *RedisSessionPresence) activityKey(userID, workerID string) string {
	return p.keys.Name("activity:" + userID + ":" + workerID)
}

// Comparison and expiry changes must share one atomic operation: detached
// reports can reach different server instances in the opposite order.
var sessionActivityWriteScript = redis.NewScript(`
local previous = redis.pcall('GET', KEYS[1])
local incoming = cjson.decode(ARGV[1])
if type(previous) == 'string' and incoming.client_at then
  local ok, record = pcall(cjson.decode, previous)
  if ok and type(record) == 'table' and type(record.client_at) == 'table' then
    local old = record.client_at
    local new = incoming.client_at
    -- Corrupt stored timestamps carry no ordering information.
    if type(old.seconds) == 'number' and type(old.nanos) == 'number'
        and old.seconds % 1 == 0 and old.nanos % 1 == 0
        and old.nanos >= 0 and old.nanos < 1000000000 then
      if old.seconds > new.seconds or (old.seconds == new.seconds and old.nanos > new.nanos) then
        local seconds = old.seconds - new.seconds
        local window = tonumber(ARGV[3])
        if seconds < window or (seconds == window and old.nanos <= new.nanos) then
          return 0
        end
      end
    end
  end
end
if tonumber(ARGV[2]) <= 0 then
  return redis.call('DEL', KEYS[1])
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return 1
`)

func (p *RedisSessionPresence) SetActivity(userID, workerID string, busy bool, at time.Time, clientAt *time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()
	key := p.activityKey(userID, workerID)
	ttl := at.Add(sessionActivityTTL).Sub(presenceNow(p.now))
	payload, err := json.Marshal(sessionActivity{Busy: busy, At: at, ClientAt: activityTimestamp(clientAt)})
	if err != nil {
		return err
	}
	// Separate from session JSON: renewLoop replays the connection's frozen payload.
	return sessionActivityWriteScript.Run(ctx, p.client, []string{key}, payload, ttl.Milliseconds(), int64(reorderWindow/time.Second)).Err()
}

func (p *RedisSessionPresence) BusyWorkers(userID string, workerIDs []string) (map[string]bool, error) {
	busy := make(map[string]bool, len(workerIDs))
	workers := make([]string, 0, len(workerIDs))
	keys := make([]string, 0, len(workerIDs))
	for _, workerID := range workerIDs {
		if workerID == "" {
			continue
		}
		if _, seen := busy[workerID]; seen {
			continue
		}
		busy[workerID] = false
		workers = append(workers, workerID)
		keys = append(keys, p.activityKey(userID, workerID))
	}
	if len(keys) == 0 {
		return busy, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()
	values, err := p.client.MGet(ctx, keys...).Result()
	if err != nil {
		return busy, err
	}
	now := presenceNow(p.now)
	var decodeErr error
	for i, value := range values {
		if value == nil {
			continue
		}
		payload, ok := value.(string)
		if !ok {
			if decodeErr == nil {
				decodeErr = fmt.Errorf("session activity: unexpected payload type %T", value)
			}
			continue
		}
		var record sessionActivity
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			if decodeErr == nil {
				decodeErr = err
			}
			continue
		}
		busy[workers[i]] = record.Busy && now.Before(record.At.Add(sessionActivityTTL))
	}
	return busy, decodeErr
}
