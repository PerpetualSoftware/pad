package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
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

func (p *MemorySessionPresence) IsBusy(userID, workerID string) (bool, error) {
	if workerID == "" {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := sessionActivityKey{userID, workerID}
	record, ok := p.activity[key]
	if !ok {
		return false, nil
	}
	if !presenceNow(p.now).Before(record.At.Add(sessionActivityTTL)) {
		delete(p.activity, key)
		return false, nil
	}
	return record.Busy, nil
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

func (p *RedisSessionPresence) IsBusy(userID, workerID string) (bool, error) {
	if workerID == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()
	payload, err := p.client.Get(ctx, p.activityKey(userID, workerID)).Bytes()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record sessionActivity
	if err := json.Unmarshal(payload, &record); err != nil {
		return false, err
	}
	return record.Busy && presenceNow(p.now).Before(record.At.Add(sessionActivityTTL)), nil
}
