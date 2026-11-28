package main

import (
	"strings"
	"testing"
)

func TestBUG3722_RedisOptionsFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		url          string
		password     string
		wantPassword string
		wantUsername string
		wantAddr     string
		wantDB       int
		wantTLS      bool
		wantErr      bool
	}{
		{name: "URL reserved characters", url: "redis://redis:6379", password: "abc/def=", wantPassword: "abc/def=", wantAddr: "redis:6379"},
		{name: "quotes spaces and punctuation", url: "redis://redis:6379", password: `a"b c@d:e#f?g%h`, wantPassword: `a"b c@d:e#f?g%h`, wantAddr: "redis:6379"},
		{name: "URL password fallback", url: "redis://:fromurl@redis:6379", wantPassword: "fromurl", wantAddr: "redis:6379"},
		{name: "password override", url: "redis://:fromurl@redis:6379", password: "override", wantPassword: "override", wantAddr: "redis:6379"},
		{name: "ACL username preserved", url: "redis://user:fromurl@redis:6379", password: "override", wantPassword: "override", wantUsername: "user", wantAddr: "redis:6379"},
		{name: "TLS and database preserved", url: "rediss://redis:6380/2", password: "p", wantPassword: "p", wantAddr: "redis:6380", wantDB: 2, wantTLS: true},
		{name: "invalid URL hides password", url: "not a url", password: "secret-value", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts, err := redisOptionsFromEnv(tt.url, tt.password)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected invalid URL error")
				}
				if !strings.Contains(err.Error(), "invalid PAD_REDIS_URL") {
					t.Error("error does not identify invalid PAD_REDIS_URL")
				}
				if strings.Contains(err.Error(), tt.password) {
					t.Error("error includes the separate password")
				}
				return
			}
			if err != nil {
				t.Fatal("unexpected Redis options error")
			}
			if opts.Password != tt.wantPassword {
				t.Error("password does not match expected value")
			}
			if opts.Username != tt.wantUsername {
				t.Errorf("Username = %q, want %q", opts.Username, tt.wantUsername)
			}
			if opts.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", opts.Addr, tt.wantAddr)
			}
			if opts.DB != tt.wantDB {
				t.Errorf("DB = %d, want %d", opts.DB, tt.wantDB)
			}
			if (opts.TLSConfig != nil) != tt.wantTLS {
				t.Errorf("TLS enabled = %t, want %t", opts.TLSConfig != nil, tt.wantTLS)
			}
		})
	}
}
