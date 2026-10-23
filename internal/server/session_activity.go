package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const sessionActivityTTL = 30 * time.Minute

type sessionActivityKey struct{ userID, workerID string }
type sessionActivity struct {
	Busy bool      `json:"busy"`
	At   time.Time `json:"at"`
}

func presenceNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

func (p *MemorySessionPresence) SetActivity(userID, workerID string, busy bool, at time.Time) error {
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
	if !now.Before(at.Add(sessionActivityTTL)) {
		delete(p.activity, key)
		return nil
	}
	p.activity[key] = sessionActivity{Busy: busy, At: at}
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

func (p *RedisSessionPresence) SetActivity(userID, workerID string, busy bool, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()
	key := p.activityKey(userID, workerID)
	ttl := at.Add(sessionActivityTTL).Sub(presenceNow(p.now))
	if ttl <= 0 {
		return p.client.Del(ctx, key).Err()
	}
	payload, err := json.Marshal(sessionActivity{Busy: busy, At: at})
	if err != nil {
		return err
	}
	// Separate from session JSON: renewLoop replays the connection's frozen payload.
	return p.client.Set(ctx, key, payload, ttl).Err()
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
