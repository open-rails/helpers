package deps

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig selects the Redis topology without code changes: one address is
// a plain client, MasterName with Sentinel addresses is a failover client, and
// several addresses without MasterName is a cluster client.
type RedisConfig struct {
	Addrs            []string `koanf:"addrs" yaml:"addrs" json:"addrs"`
	MasterName       string   `koanf:"master_name" yaml:"master_name" json:"master_name"`
	Username         string   `koanf:"username" yaml:"username" json:"username"`
	Password         string   `koanf:"password" yaml:"password" json:"password"`
	SentinelUsername string   `koanf:"sentinel_username" yaml:"sentinel_username" json:"sentinel_username"`
	SentinelPassword string   `koanf:"sentinel_password" yaml:"sentinel_password" json:"sentinel_password"`
	DB               int      `koanf:"db" yaml:"db" json:"db"`
}

// Configured reports whether any address is set.
func (c RedisConfig) Configured() bool {
	for _, a := range c.Addrs {
		if strings.TrimSpace(a) != "" {
			return true
		}
	}
	return false
}

// NewRedis builds a client without contacting the server. Timeouts are short
// and retries are off: the supervisor, not each command, owns reconnection.
func NewRedis(c RedisConfig) redis.UniversalClient {
	var addrs []string
	for _, a := range c.Addrs {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:            addrs,
		MasterName:       strings.TrimSpace(c.MasterName),
		Username:         c.Username,
		Password:         c.Password,
		SentinelUsername: c.SentinelUsername,
		SentinelPassword: c.SentinelPassword,
		DB:               c.DB,
		DialTimeout:      500 * time.Millisecond,
		ReadTimeout:      500 * time.Millisecond,
		WriteTimeout:     500 * time.Millisecond,
		PoolTimeout:      time.Second,
		DialerRetries:    1,
		MaxRetries:       -1,
	})
}

// AddRedis supervises client as an optional dependency.
func (s *Supervisor) AddRedis(name string, client redis.UniversalClient) *Dependency {
	return s.Add(name, Optional, func(ctx context.Context) error { return client.Ping(ctx).Err() }, RedisUnavailable)
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
		strings.Contains(msg, "all sentinels are unreachable") || strings.Contains(msg, "no such host")
}
