package deps

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig selects the Redis topology without code changes: MasterName with
// SentinelAddrs is a Sentinel failover client; otherwise one address is a plain
// client and several addresses a cluster client.
type RedisConfig struct {
	Addrs            []string `koanf:"addrs" yaml:"addrs" json:"addrs"`
	SentinelAddrs    []string `koanf:"sentinel_addrs" yaml:"sentinel_addrs" json:"sentinel_addrs"`
	MasterName       string   `koanf:"master_name" yaml:"master_name" json:"master_name"`
	Username         string   `koanf:"username" yaml:"username" json:"username"`
	Password         string   `koanf:"password" yaml:"password" json:"password"`
	SentinelUsername string   `koanf:"sentinel_username" yaml:"sentinel_username" json:"sentinel_username"`
	// SentinelPassword defaults to Password.
	SentinelPassword string `koanf:"sentinel_password" yaml:"sentinel_password" json:"sentinel_password"`
	DB               int    `koanf:"db" yaml:"db" json:"db"`
}

func (c RedisConfig) sentinel() bool { return strings.TrimSpace(c.MasterName) != "" }

func (c RedisConfig) addrs() []string {
	src := c.Addrs
	if c.sentinel() {
		src = c.SentinelAddrs
	}
	var out []string
	for _, a := range src {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Configured reports whether Redis is configured at all.
func (c RedisConfig) Configured() bool { return len(c.addrs()) > 0 }

// Validate rejects a half-configured Sentinel setup.
func (c RedisConfig) Validate() error {
	switch {
	case c.sentinel() && len(c.addrs()) == 0:
		return errors.New("redis: master_name requires sentinel_addrs")
	case !c.sentinel() && len(c.SentinelAddrs) > 0:
		return errors.New("redis: sentinel_addrs requires master_name")
	}
	return nil
}

// NewRedis builds a client without contacting the server. Timeouts are short
// and retries are off: the supervisor, not each command, owns reconnection.
func NewRedis(c RedisConfig) redis.UniversalClient {
	sentinelPassword := c.SentinelPassword
	if sentinelPassword == "" {
		sentinelPassword = c.Password
	}
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:            c.addrs(),
		MasterName:       strings.TrimSpace(c.MasterName),
		Username:         c.Username,
		Password:         c.Password,
		SentinelUsername: c.SentinelUsername,
		SentinelPassword: sentinelPassword,
		DB:               c.DB,
		DialTimeout:      500 * time.Millisecond,
		ReadTimeout:      500 * time.Millisecond,
		WriteTimeout:     500 * time.Millisecond,
		PoolTimeout:      time.Second,
		DialerRetries:    1,
		MaxRetries:       -1,
	})
}

// AddRedis supervises client as an optional dependency. The probe writes a
// short-lived key, so a primary that refuses writes (no replica connected,
// read-only after failover) counts as down, not only an unreachable one.
func (s *Supervisor) AddRedis(name string, client redis.UniversalClient) *Dependency {
	key := "deps:probe:" + name
	return s.Add(name, Optional, func(ctx context.Context) error {
		return client.Set(ctx, key, "1", 10*time.Second).Err()
	}, RedisUnavailable)
}

// RedisUnavailable reports whether err from a Redis command means the server
// is unreachable, as opposed to a miss (redis.Nil) or a command error.
func RedisUnavailable(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, redis.TxFailedErr) {
		return false
	}
	if errors.Is(err, redis.ErrClosed) || errors.Is(err, redis.ErrPoolTimeout) || IsConnectivity(err) {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "LOADING") || strings.HasPrefix(msg, "READONLY") ||
		strings.HasPrefix(msg, "MASTERDOWN") || strings.HasPrefix(msg, "CLUSTERDOWN") ||
		strings.HasPrefix(msg, "NOREPLICAS") ||
		strings.Contains(msg, "all sentinels are unreachable") || strings.Contains(msg, "no such host")
}
