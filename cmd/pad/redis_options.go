package main

import (
	"fmt"

	"github.com/redis/go-redis/v9"
)

// redisOptionsFromEnv parses the Redis URL and, when password is non-empty,
// sets it as the connection password, replacing any password in the URL.
// The password travels outside the URL so that it needs no percent-encoding:
// any byte sequence is used as given.
func redisOptionsFromEnv(redisURL, password string) (*redis.Options, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid PAD_REDIS_URL: %w", err)
	}
	if password != "" {
		opts.Password = password
	}
	return opts, nil
}
