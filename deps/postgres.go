package deps

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresUnavailable reports whether err means Postgres could not be reached
// or is not accepting work right now (connection failures, timeouts, server
// shutdown/startup), as opposed to a query or schema error.
func PostgresUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if IsConnectivity(err) || pgconn.Timeout(err) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") || // connection exception
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03" || // shutdown, cannot connect now
			pgErr.Code == "53300" // too many connections
	}
	return strings.Contains(err.Error(), "closed pool") || strings.Contains(err.Error(), "conn closed")
}

// PostgresProbe checks Postgres over one dedicated connection, so a saturated
// application pool never reads as Postgres being down. The connection is
// re-dialed after any failure. Pass close to Add as OnClose.
func PostgresProbe(cfg *pgx.ConnConfig) (probe Probe, close func()) {
	var mu sync.Mutex
	var conn *pgx.Conn
	probe = func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if conn == nil {
			c, err := pgx.ConnectConfig(ctx, cfg.Copy())
			if err != nil {
				return err
			}
			conn = c
		}
		if err := conn.Ping(ctx); err != nil {
			_ = conn.Close(context.Background())
			conn = nil
			return err
		}
		return nil
	}
	close = func() {
		mu.Lock()
		defer mu.Unlock()
		if conn != nil {
			_ = conn.Close(context.Background())
			conn = nil
		}
	}
	return probe, close
}

// AddPostgres supervises Postgres as a required dependency over a dedicated
// connection that is closed when the dependency stops.
func (s *Supervisor) AddPostgres(name string, cfg *pgx.ConnConfig, opts ...DepOption) *Dependency {
	probe, closeConn := PostgresProbe(cfg)
	return s.Add(name, Required, probe, PostgresUnavailable, append(opts, OnClose(closeConn))...)
}
