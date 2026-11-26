package river

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// NewWithSession is New for a pool behind a transaction pooler (PgBouncer in
// transaction mode): the client works through pool and LISTENs on a connection
// of its own dialed from session, a direct configuration of the same database
// and role. A nil session is New.
//
// The job table stays the truth. A notification only wakes producers, which
// also poll every FetchPollInterval; River pings the listener every 5 s and
// redials it after an error, so a notification lost meanwhile delays a job and
// loses none. pool must not rely on server-side prepared statements across
// round trips: its DefaultQueryExecMode is CacheDescribe, Exec or
// SimpleProtocol.
func NewWithSession(ctx context.Context, pool *pgxpool.Pool, session *pgxpool.Config, options *riverqueue.Config, jobs ...Contribution) (*riverqueue.Client[pgx.Tx], error) {
	if session != nil && session.ConnConfig == nil {
		return nil, errors.New("riverkit: session configuration has no connection settings")
	}
	return compose(ctx, pool, session, options, jobs)
}

// Driver is River's pgx driver for pool. With a session configuration its
// listener dials its own connection from session instead of holding one of
// pool's; nil is riverpgxv5.New(pool).
func Driver(pool *pgxpool.Pool, session *pgxpool.Config) riverdriver.Driver[pgx.Tx] {
	if session == nil {
		return riverpgxv5.New(pool)
	}
	return &sessionDriver{Driver: riverpgxv5.New(pool), session: session.Copy()}
}

// checkPooled refuses exec modes that run one query's prepare and execute in
// separate round trips or keep named statements: a transaction pooler may hand
// each round trip to another server connection.
func checkPooled(pool *pgxpool.Pool) error {
	switch mode := pool.Config().ConnConfig.DefaultQueryExecMode; mode {
	case pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol:
		return nil
	default:
		return fmt.Errorf("riverkit: a transaction-pooled pool needs DefaultQueryExecMode cache_describe, exec or simple_protocol, not %s", mode)
	}
}

type sessionDriver struct {
	*riverpgxv5.Driver
	session *pgxpool.Config
}

func (d *sessionDriver) GetListener(params *riverdriver.GetListenenerParams) riverdriver.Listener {
	return &sessionListener{cfg: d.session, schema: params.Schema}
}

// sessionListener is riverpgxv5's listener on a connection it dials and
// closes itself.
type sessionListener struct {
	cfg *pgxpool.Config

	mu        sync.Mutex
	conn      *pgx.Conn
	schema    string
	prefix    string
	afterExec string
}

func (l *sessionListener) Connect(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		return errors.New("riverkit: listener already connected")
	}
	cfg := l.cfg.ConnConfig.Copy()
	if l.cfg.BeforeConnect != nil {
		if err := l.cfg.BeforeConnect(ctx, cfg); err != nil {
			return err
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = conn.Close(context.WithoutCancel(ctx))
		return err
	}
	if l.cfg.AfterConnect != nil {
		if err := l.cfg.AfterConnect(ctx, conn); err != nil {
			return fail(err)
		}
	}
	if l.afterExec != "" {
		if _, err := conn.Exec(ctx, l.afterExec); err != nil {
			return fail(err)
		}
	}
	if l.schema == "" {
		if err := conn.QueryRow(ctx, "SELECT coalesce(current_schema(), '')").Scan(&l.schema); err != nil {
			return fail(err)
		}
	}
	if l.schema != "" {
		l.prefix = l.schema + "."
	}
	l.conn = conn
	return nil
}

func (l *sessionListener) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil
	}
	err := l.conn.Close(ctx)
	l.conn = nil
	return err
}

func (l *sessionListener) Listen(ctx context.Context, topic string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.conn.Exec(ctx, "LISTEN "+pgx.Identifier{l.prefix + topic}.Sanitize())
	return err
}

func (l *sessionListener) Unlisten(ctx context.Context, topic string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.conn.Exec(ctx, "UNLISTEN "+pgx.Identifier{l.prefix + topic}.Sanitize())
	return err
}

// pingTimeout bounds River's listener ping, which carries no deadline: a link
// that stalls without a reset would hang the notifier until TCP gives up. A
// failed ping closes the connection and River redials it.
const pingTimeout = 5 * time.Second

func (l *sessionListener) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn.Ping(ctx)
}

func (l *sessionListener) Schema() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.schema
}

func (l *sessionListener) SetAfterConnectExec(sql string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.afterExec = sql
}

func (l *sessionListener) WaitForNotification(ctx context.Context) (*riverdriver.Notification, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return nil, err
	}
	return &riverdriver.Notification{Topic: strings.TrimPrefix(n.Channel, l.prefix), Payload: n.Payload}, nil
}
