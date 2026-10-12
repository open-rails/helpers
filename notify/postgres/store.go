package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/notify"
)

// Config binds a store to one local application/tenant scope in a migrated schema.
type Config struct{ Schema, Scope string }

// Store retains notices and actionable items. Scope and recipients are data
// isolation boundaries; authorization remains with the host.
type Store struct {
	pool                     *pgxpool.Pool
	scope, table, recipients string
}

// New binds an existing pool; it neither migrates nor opens connections.
func New(pool *pgxpool.Pool, cfg Config) (*Store, error) {
	name, err := schemaName(cfg.Schema)
	if err != nil {
		return nil, err
	}
	if pool == nil || strings.TrimSpace(cfg.Scope) == "" || len(cfg.Scope) > 512 || strings.ContainsRune(cfg.Scope, 0) {
		return nil, fmt.Errorf("%w: pool and scope required", notify.ErrInvalid)
	}
	return &Store{pool: pool, scope: cfg.Scope, table: name + `.notify_items`, recipients: name + `.notify_recipients`}, nil
}

const columns = `id,audience,recipient,source,source_scope,source_id,kind,data,created_at,read_at,status,severity,resolved_at,updated_at,revision,action_requested,source_subject,dismissed_at`

func scan(row pgx.Row) (notify.Item, error) {
	var n notify.Item
	err := row.Scan(&n.ID, &n.Recipient.Audience, &n.Recipient.ID, &n.Origin.System, &n.Origin.Scope, &n.Origin.ID, &n.Kind, &n.Data, &n.CreatedAt, &n.ReadAt, &n.Status, &n.Severity, &n.ResolvedAt, &n.UpdatedAt, &n.Revision, &n.ActionRequested, &n.Origin.Subject, &n.DismissedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, notify.ErrNotFound
	}
	// An older producer can still insert workflow items after migration 2.
	// Their nonempty status already establishes the action-request category.
	if n.Status != notify.StatusNotice {
		n.ActionRequested = true
	}
	return n, err
}

// lock takes a real row lock, so an older repeatable-read snapshot fails with
// a serialization error when erasure changed the guard after its snapshot.
// An advisory lock alone cannot make that snapshot see an erasure tombstone.
func (s *Store) lock(ctx context.Context, tx pgx.Tx, r notify.Recipient) (bool, error) {
	if err := notify.ValidateRecipient(r); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.recipients+`(scope,audience,recipient) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.scope, r.Audience, r.ID); err != nil {
		return false, err
	}
	var erased bool
	err := tx.QueryRow(ctx, `SELECT erased_at IS NOT NULL FROM `+s.recipients+` WHERE scope=$1 AND audience=$2 AND recipient=$3 FOR UPDATE`, s.scope, r.Audience, r.ID).Scan(&erased)
	return erased, err
}

// Publish inserts once per origin and recipient. Retries preserve content and
// every local read/workflow transition, including after a crash before an ack.
func (s *Store) Publish(ctx context.Context, in notify.Input) (notify.Item, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return notify.Item{}, err
	}
	defer tx.Rollback(ctx)
	out, err := s.PublishTx(ctx, tx, in)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// PublishTx joins the domain change's transaction. The transaction must reach
// the same database as the store; the caller commits or rolls it back.
func (s *Store) PublishTx(ctx context.Context, tx pgx.Tx, in notify.Input) (notify.Item, error) {
	in, err := notify.Validate(in)
	if err != nil {
		return notify.Item{}, err
	}
	if tx == nil {
		return notify.Item{}, fmt.Errorf("%w: transaction required", notify.ErrInvalid)
	}
	erased, err := s.lock(ctx, tx, in.Recipient)
	if err != nil {
		return notify.Item{}, err
	}
	if erased {
		return notify.Item{}, notify.ErrRecipientErased
	}
	_, err = tx.Exec(ctx, `INSERT INTO `+s.table+`(scope,id,audience,recipient,source,source_scope,source_id,kind,data,created_at,status,severity,action_requested,source_subject) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(scope,audience,recipient,source,source_scope,source_id) DO NOTHING`, s.scope, rand.Text(), in.Recipient.Audience, in.Recipient.ID, in.Origin.System, in.Origin.Scope, in.Origin.ID, in.Kind, in.Data, in.CreatedAt, in.Status, in.Severity, in.ActionRequested, in.Origin.Subject)
	if err != nil {
		return notify.Item{}, fmt.Errorf("notify: publish: %w", err)
	}
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM `+s.table+` WHERE scope=$1 AND audience=$2 AND recipient=$3 AND source=$4 AND source_scope=$5 AND source_id=$6`, s.scope, in.Recipient.Audience, in.Recipient.ID, in.Origin.System, in.Origin.Scope, in.Origin.ID))
}

type cursor struct {
	Scope      string
	Recipients string
	Unread     bool
	Status     *notify.Status
	Dismissed  bool
	At         time.Time
	ID         string
}

// List reads one authorized recipient's inbox, newest first. Restart scans from
// an empty cursor to discover late commits; cursors are not delivery watermarks.
func (s *Store) List(ctx context.Context, r notify.Recipient, o notify.ListOptions) (notify.Page, error) {
	return s.ListRecipients(ctx, []notify.Recipient{r}, o)
}

// ListRecipients reads only the recipients the host authorized for this request.
// Filtering happens in SQL before pagination, so an unlinked recipient consumes
// no page slots. Empty recipients returns an empty page. Cursors bind the
// normalized recipient set and filters; restart paging after access changes.
func (s *Store) ListRecipients(ctx context.Context, recipients []notify.Recipient, o notify.ListOptions) (notify.Page, error) {
	out := notify.Page{Items: []notify.Item{}}
	audiences, ids, key, err := recipientSet(recipients)
	if err != nil {
		return out, err
	}
	if o.Limit < 0 || o.Limit > 100 {
		return out, fmt.Errorf("%w: limit must be 0-100", notify.ErrInvalid)
	}
	if o.Limit == 0 {
		o.Limit = 20
	}
	if o.Status != nil && !validStatus(*o.Status) {
		return out, fmt.Errorf("%w: status", notify.ErrInvalid)
	}
	var c cursor
	if o.Cursor != "" {
		if len(o.Cursor) > 4096 {
			return out, fmt.Errorf("%w: cursor", notify.ErrInvalid)
		}
		b, err := base64.RawURLEncoding.DecodeString(o.Cursor)
		if err != nil || json.Unmarshal(b, &c) != nil || c.Scope != s.scope || c.Recipients != key || c.Unread != o.UnreadOnly || c.Dismissed != o.Dismissed || !sameStatus(c.Status, o.Status) || c.At.IsZero() || c.ID == "" {
			return out, fmt.Errorf("%w: cursor", notify.ErrInvalid)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	var at *time.Time
	if o.Cursor != "" {
		at = &c.At
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM `+s.table+` WHERE scope=$1 AND (audience,recipient) IN (SELECT * FROM unnest($2::text[],$3::text[])) AND (NOT $4 OR read_at IS NULL) AND ($5::text IS NULL OR status=$5) AND ($6::timestamptz IS NULL OR (created_at,id)<($6,$7)) AND (dismissed_at IS NOT NULL)=$9 ORDER BY created_at DESC,id DESC LIMIT $8`, s.scope, audiences, ids, o.UnreadOnly, o.Status, at, c.ID, o.Limit+1, o.Dismissed)
	if err != nil {
		return out, fmt.Errorf("notify: list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, n)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > o.Limit {
		out.Items = out.Items[:o.Limit]
		last := out.Items[len(out.Items)-1]
		b, _ := json.Marshal(cursor{Scope: s.scope, Recipients: key, Unread: o.UnreadOnly, Status: o.Status, Dismissed: o.Dismissed, At: last.CreatedAt, ID: last.ID})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

func recipientSet(in []notify.Recipient) ([]string, []string, string, error) {
	if len(in) > 10000 {
		return nil, nil, "", fmt.Errorf("%w: at most 10000 authorized recipients", notify.ErrInvalid)
	}
	set := make(map[notify.Recipient]struct{}, len(in))
	for _, r := range in {
		if err := notify.ValidateRecipient(r); err != nil {
			return nil, nil, "", err
		}
		set[r] = struct{}{}
	}
	rs := make([]notify.Recipient, 0, len(set))
	for r := range set {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Audience != rs[j].Audience {
			return rs[i].Audience < rs[j].Audience
		}
		return rs[i].ID < rs[j].ID
	})
	audiences, ids := make([]string, 0, len(rs)), make([]string, 0, len(rs))
	for _, r := range rs {
		audiences = append(audiences, string(r.Audience))
		ids = append(ids, r.ID)
	}
	b, _ := json.Marshal(rs)
	digest := sha256.Sum256(b)
	return audiences, ids, fmt.Sprintf("%x", digest), nil
}

func sameStatus(a, b *notify.Status) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func validStatus(v notify.Status) bool {
	return v == notify.StatusNotice || v == notify.StatusOpen || v == notify.StatusResolved || v == notify.StatusDismissed
}

// SetRead changes only local read state. It never resolves an actionable item.
func (s *Store) SetRead(ctx context.Context, r notify.Recipient, id string, read bool) (notify.Item, error) {
	return s.SetReadRecipients(ctx, []notify.Recipient{r}, id, read)
}

// Transition changes workflow state under optimistic concurrency. The caller
// must perform/authorize any domain action first, or use TransitionTx in the
// same transaction as that action. General notices cannot become work items.
func (s *Store) Transition(ctx context.Context, r notify.Recipient, id string, revision int64, status notify.Status) (notify.Item, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return notify.Item{}, err
	}
	defer tx.Rollback(ctx)
	out, err := s.TransitionTx(ctx, tx, r, id, revision, status)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// TransitionTx joins a host's transaction; reading and resolving remain separate.
func (s *Store) TransitionTx(ctx context.Context, tx pgx.Tx, r notify.Recipient, id string, revision int64, status notify.Status) (notify.Item, error) {
	if err := notify.ValidateRecipient(r); err != nil {
		return notify.Item{}, err
	}
	if tx == nil || revision < 1 || status == notify.StatusNotice || !validStatus(status) {
		return notify.Item{}, fmt.Errorf("%w: workflow transition", notify.ErrInvalid)
	}
	erased, err := s.lock(ctx, tx, r)
	if err != nil {
		return notify.Item{}, err
	}
	if erased {
		return notify.Item{}, notify.ErrRecipientErased
	}
	current, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM `+s.table+` WHERE scope=$1 AND audience=$2 AND recipient=$3 AND id=$4 FOR UPDATE`, s.scope, r.Audience, r.ID, id))
	if err != nil {
		return current, err
	}
	if current.Revision != revision {
		return notify.Item{}, notify.ErrConflict
	}
	if current.Status == notify.StatusNotice {
		return notify.Item{}, fmt.Errorf("%w: general notice has no workflow", notify.ErrInvalid)
	}
	if current.Status == status {
		return current, nil
	}
	return scan(tx.QueryRow(ctx, `UPDATE `+s.table+` SET status=$5,resolved_at=CASE WHEN $5='open' THEN NULL ELSE clock_timestamp() END,updated_at=clock_timestamp(),revision=revision+1 WHERE scope=$1 AND audience=$2 AND recipient=$3 AND id=$4 RETURNING `+columns, s.scope, r.Audience, r.ID, id, status))
}

// EraseRecipient deletes an inbox and permanently fences delayed publications.
// This is account erasure, not the operation for dismissing one item.
func (s *Store) EraseRecipient(ctx context.Context, r notify.Recipient) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.EraseRecipientTx(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EraseRecipientTx joins account erasure in the host's transaction.
func (s *Store) EraseRecipientTx(ctx context.Context, tx pgx.Tx, r notify.Recipient) error {
	if tx == nil {
		return fmt.Errorf("%w: transaction required", notify.ErrInvalid)
	}
	if _, err := s.lock(ctx, tx, r); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.recipients+` SET erased_at=coalesce(erased_at,clock_timestamp()) WHERE scope=$1 AND audience=$2 AND recipient=$3`, s.scope, r.Audience, r.ID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM `+s.table+` WHERE scope=$1 AND audience=$2 AND recipient=$3`, s.scope, r.Audience, r.ID)
	return err
}
