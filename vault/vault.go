// Package vault keeps one HashiCorp Vault client authenticated for a host. It
// logs in with a token, AppRole or Kubernetes, renews the token while Vault
// allows, and logs in again when renewal ends or Vault refuses the token.
//
// It reads no environment variables and no files: a credential that rotates
// (an AppRole secret_id, a service-account token) comes from a function the
// host supplies, called for every login.
package vault

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// Method is how a Session authenticates.
type Method string

const (
	MethodToken      Method = "token"
	MethodAppRole    Method = "approle"
	MethodKubernetes Method = "kubernetes"
)

// DefaultTimeout bounds one request when Config.Timeout is 0.
const DefaultTimeout = 10 * time.Second

// Config is a Vault server and the credentials a Session logs in with.
type Config struct {
	// Address is the server URL; required, with no environment default.
	Address   string
	Namespace string
	// Method is MethodToken when empty with Token set.
	Method Method
	// Token is a pre-issued token (MethodToken). It is renewed while Vault
	// allows; it cannot be issued again.
	Token  string
	RoleID string
	// SecretID returns the AppRole secret_id; called for every login, so a
	// rotated secret is used.
	SecretID       func(context.Context) (string, error)
	KubernetesRole string
	// KubernetesJWT returns the service-account token; called for every
	// login.
	KubernetesJWT func(context.Context) (string, error)
	// Mount is the auth method's mount (the -path of `vault auth enable`,
	// without auth/); empty is the method's name.
	Mount string
	// TLS configures the client's TLS (a private CA); nil uses the system
	// roots.
	TLS *tls.Config
	// Timeout bounds one request; 0 is DefaultTimeout.
	Timeout time.Duration
	// Logger is nil for slog.Default(), read once in Login.
	Logger *slog.Logger
}

// ErrNotAuthenticated is Probe's answer while the Session holds no working
// token.
var ErrNotAuthenticated = errors.New("vault: not authenticated")

// Login retry pacing: capped full-jitter exponential backoff.
var (
	backoffBase = time.Second
	backoffMax  = time.Minute
)

const lookupSelfPath = "/v1/auth/token/lookup-self"

// Session keeps Client authenticated until Close or the Login context ends.
type Session struct {
	client *vaultapi.Client
	// login carries no token: a dead token never rides on a login.
	login     *vaultapi.Client
	cfg       Config
	loginPath string
	log       *slog.Logger
	// check asks for a token self-lookup after a 403; buffered(1) coalesces.
	check  chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error
}

// Login validates cfg and returns the session without waiting on the
// network: it logs in on its own goroutine at once (capped full-jitter
// backoff on failure, forever), renews the token while Vault allows, logs in
// again when renewal ends, and logs in again at once when a request made
// through Client is answered 403 and a self-lookup of the token is refused
// too. A 403 whose self-lookup succeeds is an ordinary policy denial. A static
// token is looked up and renewed while renewable; it cannot be issued again.
// The session runs until Close or ctx ends. Only a configuration error fails
// Login.
func Login(ctx context.Context, cfg Config) (*Session, error) {
	cfg, err := normalize(cfg)
	if err != nil {
		return nil, err
	}
	s := &Session{cfg: cfg, log: cfg.Logger, check: make(chan struct{}, 1), done: make(chan struct{}), err: ErrNotAuthenticated}
	if s.log == nil {
		s.log = slog.Default()
	}
	if cfg.Method != MethodToken {
		s.loginPath = "auth/" + cfg.Mount + "/login"
	}
	base := transport(cfg.TLS)
	if s.client, err = newClient(cfg, &observer{next: base, check: s.check, login: s.loginPath}); err != nil {
		return nil, err
	}
	if s.login, err = newClient(cfg, base); err != nil {
		return nil, err
	}
	if cfg.Method == MethodToken {
		s.client.SetToken(strings.TrimSpace(cfg.Token))
	}
	ctx, s.cancel = context.WithCancel(ctx)
	go s.run(ctx)
	return s, nil
}

// Client is the one client; a fresh token is swapped onto it in place.
func (s *Session) Client() *vaultapi.Client { return s.client }

// Probe is nil only while authenticated and a token self-lookup answers.
// Before the first login, or while logging in again fails, it returns
// ErrNotAuthenticated wrapping the last error, without a network call.
func (s *Session) Probe(ctx context.Context) error {
	if err := s.state(); err != nil {
		return err
	}
	if _, err := s.client.Auth().Token().LookupSelfWithContext(ctx); err != nil {
		if forbidden(err) {
			s.signal()
		}
		return fmt.Errorf("vault: token self-lookup: %w", err)
	}
	return nil
}

// Close stops renewal and logging in; it does not revoke the token. It is
// safe to call twice.
func (s *Session) Close() {
	s.cancel()
	<-s.done
	s.setErr(fmt.Errorf("%w: session closed", ErrNotAuthenticated))
}

func normalize(cfg Config) (Config, error) {
	cfg.Address = strings.TrimSpace(cfg.Address)
	if cfg.Address == "" {
		return cfg, errors.New("vault: Address is required")
	}
	cfg.Method = Method(strings.ToLower(strings.TrimSpace(string(cfg.Method))))
	if cfg.Method == "" && strings.TrimSpace(cfg.Token) != "" {
		cfg.Method = MethodToken
	}
	switch cfg.Method {
	case MethodToken:
		if strings.TrimSpace(cfg.Token) == "" {
			return cfg, errors.New("vault: token auth needs Token")
		}
	case MethodAppRole:
		if strings.TrimSpace(cfg.RoleID) == "" || cfg.SecretID == nil {
			return cfg, errors.New("vault: approle auth needs RoleID and SecretID")
		}
	case MethodKubernetes:
		if strings.TrimSpace(cfg.KubernetesRole) == "" || cfg.KubernetesJWT == nil {
			return cfg, errors.New("vault: kubernetes auth needs KubernetesRole and KubernetesJWT")
		}
	case "":
		return cfg, errors.New("vault: Method is required: token, approle or kubernetes")
	default:
		return cfg, fmt.Errorf("vault: unknown Method %q: token, approle or kubernetes", cfg.Method)
	}
	mount := strings.Trim(strings.TrimSpace(cfg.Mount), "/")
	if mount != "" && (strings.HasPrefix(mount, "auth/") || path.Clean(mount) != mount || strings.Contains(mount, "..")) {
		return cfg, fmt.Errorf("vault: Mount %q must be the auth method's path, without auth/", cfg.Mount)
	}
	if mount == "" {
		mount = string(cfg.Method)
	}
	cfg.Mount = mount
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return cfg, nil
}

func transport(tlsConfig *tls.Config) http.RoundTripper {
	t := &http.Transport{}
	if d, ok := http.DefaultTransport.(*http.Transport); ok {
		t = d.Clone()
	}
	if tlsConfig != nil {
		t.TLSClientConfig = tlsConfig.Clone()
	} else {
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return t
}

func newClient(cfg Config, rt http.RoundTripper) (*vaultapi.Client, error) {
	c, err := vaultapi.NewClient(&vaultapi.Config{
		Address: cfg.Address,
		Timeout: cfg.Timeout,
		HttpClient: &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("vault: client: %w", err)
	}
	// NewClient takes VAULT_TOKEN, VAULT_NAMESPACE and VAULT_HEADERS from the
	// environment; none of them applies.
	c.SetHeaders(http.Header{vaultapi.RequestHeaderName: {"true"}})
	c.ClearToken()
	if ns := strings.TrimSpace(cfg.Namespace); ns != "" {
		c.SetNamespace(ns)
	}
	return c, nil
}

// observer asks for a token check when Vault answers 403, except to the
// self-lookup and login that check it.
type observer struct {
	next  http.RoundTripper
	check chan<- struct{}
	login string
}

func (o *observer) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := o.next.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusForbidden &&
		!strings.HasSuffix(r.URL.Path, lookupSelfPath) && (o.login == "" || !strings.HasSuffix(r.URL.Path, "/v1/"+o.login)) {
		select {
		case o.check <- struct{}{}:
		default:
		}
	}
	return resp, err
}

func (s *Session) signal() {
	select {
	case s.check <- struct{}{}:
	default:
	}
}

func (s *Session) state() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Session) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *Session) run(ctx context.Context) {
	defer close(s.done)
	for failures := 0; ctx.Err() == nil; {
		secret, err := s.authenticate(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setErr(fmt.Errorf("%w: %w", ErrNotAuthenticated, err))
			level := slog.LevelDebug
			if failures == 0 {
				level = slog.LevelWarn
			}
			s.log.Log(ctx, level, "vault: login failed; retrying", "method", string(s.cfg.Method), "attempt", failures+1, "error", err.Error())
			if !sleep(ctx, backoff(failures)) {
				return
			}
			failures++
			continue
		}
		select {
		case <-s.check:
		default:
		}
		s.setErr(nil)
		s.log.Info("vault: authenticated", "method", string(s.cfg.Method), "attempts", failures+1)
		failures = 0
		s.hold(ctx, secret)
		// A lease ends within its grace period, before the token does: pace
		// the next login so a token that cannot be renewed is not looked up
		// in a loop.
		if !sleep(ctx, backoff(0)) {
			return
		}
	}
}

// authenticate logs in, or looks up a static token. A nil secret is a token
// that never expires.
func (s *Session) authenticate(ctx context.Context) (*vaultapi.Secret, error) {
	if s.cfg.Method == MethodToken {
		return s.lookupToken(ctx)
	}
	data := map[string]any{}
	switch s.cfg.Method {
	case MethodAppRole:
		id, err := s.cfg.SecretID(ctx)
		if err != nil {
			return nil, fmt.Errorf("vault: approle secret_id: %w", err)
		}
		data["role_id"], data["secret_id"] = strings.TrimSpace(s.cfg.RoleID), strings.TrimSpace(id)
	case MethodKubernetes:
		jwt, err := s.cfg.KubernetesJWT(ctx)
		if err != nil {
			return nil, fmt.Errorf("vault: kubernetes service-account token: %w", err)
		}
		data["role"], data["jwt"] = strings.TrimSpace(s.cfg.KubernetesRole), strings.TrimSpace(jwt)
	}
	secret, err := s.login.Logical().WriteWithContext(ctx, s.loginPath, data)
	if err != nil {
		return nil, fmt.Errorf("vault: %s login: %w", s.cfg.Method, err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return nil, fmt.Errorf("vault: %s login returned no token", s.cfg.Method)
	}
	s.client.SetToken(secret.Auth.ClientToken)
	return secret, nil
}

func (s *Session) lookupToken(ctx context.Context) (*vaultapi.Secret, error) {
	lookup, err := s.client.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: token self-lookup (a static token cannot be issued again): %w", err)
	}
	renewable, err := lookup.TokenIsRenewable()
	if err != nil {
		return nil, fmt.Errorf("vault: token self-lookup: %w", err)
	}
	ttl, err := lookup.TokenTTL()
	if err != nil {
		return nil, fmt.Errorf("vault: token self-lookup: %w", err)
	}
	auth := &vaultapi.SecretAuth{ClientToken: s.client.Token(), Renewable: renewable, LeaseDuration: int(ttl.Seconds())}
	switch {
	case renewable:
	case ttl > 0:
		s.log.Warn("vault: the token is not renewable; replace it before it expires", "expires_in", ttl.String())
	default:
		return nil, nil
	}
	return &vaultapi.Secret{Auth: auth}, nil
}

// hold keeps secret renewed until renewal ends, the token is refused or ctx
// ends.
func (s *Session) hold(ctx context.Context, secret *vaultapi.Secret) {
	var (
		done    <-chan error
		renewed <-chan *vaultapi.RenewOutput
	)
	if secret != nil {
		if w, err := s.client.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{Secret: secret}); err == nil {
			go w.Start()
			defer w.Stop()
			done, renewed = w.DoneCh(), w.RenewCh()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-done:
			if err != nil {
				s.log.Info("vault: token renewal ended; logging in again", "error", err.Error())
			}
			return
		case <-renewed:
		case <-s.check:
			if _, err := s.client.Auth().Token().LookupSelfWithContext(ctx); forbidden(err) {
				s.setErr(fmt.Errorf("%w: token refused: %w", ErrNotAuthenticated, err))
				s.log.Warn("vault: the token was refused; logging in again", "method", string(s.cfg.Method))
				return
			}
		}
	}
}

func forbidden(err error) bool {
	var r *vaultapi.ResponseError
	return errors.As(err, &r) && r.StatusCode == http.StatusForbidden
}

func backoff(attempt int) time.Duration {
	d := backoffMax
	if attempt < 32 {
		d = min(backoffBase<<attempt, backoffMax)
	}
	return rand.N(d) + 1
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
