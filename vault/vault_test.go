package vault

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	backoffBase, backoffMax = 5*time.Millisecond, 20*time.Millisecond
	os.Exit(m.Run())
}

// fakeVault answers the endpoints a Session and a KV v2 read use.
type fakeVault struct {
	srv *httptest.Server

	mu         sync.Mutex
	live       map[string]bool
	issued     int
	secretID   string // the approle secret_id accepted
	lease      int    // issued tokens' lease, seconds
	renewLease int    // lease a renewal grants
	renewable  bool
	logins     []map[string]any
	loginHdrs  []http.Header
	renews     int
	lookups    int
	gate       chan struct{} // non-nil holds logins until closed
}

func newFakeVault(t *testing.T, setup func(f *fakeVault)) *fakeVault {
	f := &fakeVault{live: map[string]bool{}, lease: 3600, renewLease: 3600, renewable: true}
	if setup != nil {
		setup(f)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Vault-Token")
	f.mu.Lock()
	live, gate := f.live[token], f.gate
	f.mu.Unlock()
	switch p := r.URL.Path; {
	case strings.HasSuffix(p, "/login"):
		if gate != nil {
			<-gate
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logins, f.loginHdrs = append(f.logins, body), append(f.loginHdrs, r.Header.Clone())
		if p == "/v1/auth/approle/login" && body["secret_id"] != f.secretID {
			reply(w, http.StatusBadRequest, map[string]any{"errors": []string{"invalid role or secret ID"}})
			return
		}
		f.issued++
		tok := fmt.Sprintf("tok-%d", f.issued)
		f.live[tok] = true
		reply(w, http.StatusOK, map[string]any{"auth": map[string]any{"client_token": tok, "renewable": f.renewable, "lease_duration": f.lease}})
	case p == "/v1/auth/token/lookup-self":
		f.mu.Lock()
		f.lookups++
		ttl, renewable := f.lease, f.renewable
		f.mu.Unlock()
		if !live {
			deny(w)
			return
		}
		reply(w, http.StatusOK, map[string]any{"data": map[string]any{"id": token, "ttl": ttl, "renewable": renewable}})
	case p == "/v1/auth/token/renew-self":
		f.mu.Lock()
		f.renews++
		lease := f.renewLease
		f.mu.Unlock()
		if !live {
			deny(w)
			return
		}
		reply(w, http.StatusOK, map[string]any{"auth": map[string]any{"client_token": token, "renewable": true, "lease_duration": lease}})
	case p == "/v1/secret/data/denied":
		deny(w)
	case strings.HasPrefix(p, "/v1/secret/data/"):
		if !live {
			deny(w)
			return
		}
		reply(w, http.StatusOK, map[string]any{"data": map[string]any{"data": map[string]any{"k": "v"}, "metadata": map[string]any{"version": 1}}})
	default:
		http.NotFound(w, r)
	}
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func deny(w http.ResponseWriter) {
	reply(w, http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
}

func (f *fakeVault) with(fn func(f *fakeVault)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeVault) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.logins)
}

func (f *fakeVault) count(n *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *n
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func login(t *testing.T, cfg Config) *Session {
	t.Helper()
	s, err := Login(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func authenticated(s *Session) func() bool { return func() bool { return s.state() == nil } }

func read(s *Session, path string) error {
	_, err := s.Client().KVv2("secret").Get(context.Background(), path)
	return err
}

func TestAppRoleLogsInAgainWithRotatedSecretWhenTokenDies(t *testing.T) {
	f := newFakeVault(t, func(f *fakeVault) { f.secretID = "s1" })
	var secretID atomic.Value
	secretID.Store("s1")
	var calls atomic.Int32
	s := login(t, Config{Address: f.srv.URL, Method: MethodAppRole, RoleID: "role", SecretID: func(context.Context) (string, error) {
		calls.Add(1)
		return secretID.Load().(string), nil
	}})
	eventually(t, "login", authenticated(s))
	if err := read(s, "x"); err != nil {
		t.Fatalf("read with the first token: %v", err)
	}
	// The operator rotates the secret_id and the token is revoked.
	f.with(func(f *fakeVault) { f.secretID, f.live["tok-1"] = "s2", false })
	secretID.Store("s2")
	if err := read(s, "x"); err == nil {
		t.Fatal("a revoked token read")
	}
	eventually(t, "a read with a new token", func() bool { return read(s, "x") == nil })
	if got := s.Client().Token(); got != "tok-2" {
		t.Fatalf("token %q, want tok-2", got)
	}
	f.with(func(f *fakeVault) {
		if len(f.logins) != 2 || f.logins[0]["secret_id"] != "s1" || f.logins[1]["secret_id"] != "s2" || f.logins[1]["role_id"] != "role" {
			t.Fatalf("logins %v", f.logins)
		}
	})
	if calls.Load() != 2 {
		t.Fatalf("SecretID called %d times, want once per login", calls.Load())
	}
	if err := s.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestPolicyDenialKeepsTheToken(t *testing.T) {
	f := newFakeVault(t, func(f *fakeVault) { f.secretID = "s" })
	s := login(t, Config{Address: f.srv.URL, Method: MethodAppRole, RoleID: "role", SecretID: func(context.Context) (string, error) { return "s", nil }})
	eventually(t, "login", authenticated(s))
	before := f.count(&f.lookups)
	if err := read(s, "denied"); err == nil {
		t.Fatal("a denied path read")
	}
	eventually(t, "the token check", func() bool { return f.count(&f.lookups) > before })
	time.Sleep(50 * time.Millisecond)
	if n := f.loginCount(); n != 1 || s.state() != nil {
		t.Fatalf("%d logins, state %v; a policy denial must keep the token", n, s.state())
	}
}

func TestKubernetesLogsInWithTheServiceAccountToken(t *testing.T) {
	f := newFakeVault(t, nil)
	var calls atomic.Int32
	s := login(t, Config{Address: f.srv.URL, Namespace: "team", Method: MethodKubernetes, KubernetesRole: "app", Mount: "/k8s-prod/",
		KubernetesJWT: func(context.Context) (string, error) { calls.Add(1); return "jwt-1\n", nil }})
	eventually(t, "login", authenticated(s))
	f.with(func(f *fakeVault) {
		if len(f.logins) != 1 || f.logins[0]["role"] != "app" || f.logins[0]["jwt"] != "jwt-1" {
			t.Fatalf("logins %v", f.logins)
		}
		if ns := f.loginHdrs[0].Get("X-Vault-Namespace"); ns != "team" {
			t.Fatalf("namespace %q", ns)
		}
	})
	if calls.Load() != 1 {
		t.Fatalf("KubernetesJWT called %d times", calls.Load())
	}
}

func TestStaticTokenIsRenewed(t *testing.T) {
	f := newFakeVault(t, func(f *fakeVault) { f.live["static"] = true })
	s := login(t, Config{Address: f.srv.URL, Token: "static"})
	eventually(t, "lookup", authenticated(s))
	eventually(t, "renewal", func() bool { return f.count(&f.renews) > 0 })
	if err := s.Probe(context.Background()); err != nil || s.Client().Token() != "static" {
		t.Fatalf("Probe %v, token %q", err, s.Client().Token())
	}
	if f.loginCount() != 0 {
		t.Fatal("a static token logged in")
	}
	// Revoked, it cannot be issued again: the session reports it.
	f.with(func(f *fakeVault) { f.live["static"] = false })
	if err := s.Probe(context.Background()); err == nil {
		t.Fatal("Probe passed on a revoked token")
	}
	eventually(t, "refusal", func() bool { return errors.Is(s.state(), ErrNotAuthenticated) })
}

func TestRenewalEndLogsInAgain(t *testing.T) {
	f := newFakeVault(t, func(f *fakeVault) { f.secretID, f.lease, f.renewLease = "s", 2, 0 })
	s := login(t, Config{Address: f.srv.URL, Method: MethodAppRole, RoleID: "role", SecretID: func(context.Context) (string, error) { return "s", nil }})
	eventually(t, "a second login", func() bool { return f.count(&f.issued) >= 2 })
	f.with(func(f *fakeVault) { f.renewLease = 3600 })
	if f.count(&f.renews) == 0 {
		t.Fatal("no renewal before logging in again")
	}
	eventually(t, "login", authenticated(s))
}

func TestProbeBeforeLoginMakesNoRequest(t *testing.T) {
	gate := make(chan struct{})
	f := newFakeVault(t, func(f *fakeVault) { f.secretID, f.gate = "s", gate })
	s := login(t, Config{Address: f.srv.URL, Method: MethodAppRole, RoleID: "role", SecretID: func(context.Context) (string, error) { return "s", nil }})
	if err := s.Probe(context.Background()); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("Probe before login: %v", err)
	}
	if n := f.count(&f.lookups); n != 0 {
		t.Fatalf("%d lookups before login", n)
	}
	close(gate)
	eventually(t, "Probe", func() bool { return s.Probe(context.Background()) == nil })
}

func TestLoginRetriesUntilVaultAccepts(t *testing.T) {
	f := newFakeVault(t, func(f *fakeVault) { f.secretID = "right" })
	var calls atomic.Int32
	s := login(t, Config{Address: f.srv.URL, Method: MethodAppRole, RoleID: "role", SecretID: func(context.Context) (string, error) {
		if calls.Add(1) < 3 {
			return "wrong", nil
		}
		return "right", nil
	}})
	eventually(t, "login", authenticated(s))
	if n := f.loginCount(); n != 3 {
		t.Fatalf("%d logins, want 3", n)
	}
}

func TestEnvironmentIsIgnored(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "env-token")
	t.Setenv("VAULT_NAMESPACE", "env-ns")
	t.Setenv("VAULT_HEADERS", `{"X-Env":"1"}`)
	f := newFakeVault(t, func(f *fakeVault) { f.secretID = "s" })
	s := login(t, Config{Address: f.srv.URL, Method: MethodAppRole, RoleID: "role", SecretID: func(context.Context) (string, error) { return "s", nil }})
	eventually(t, "login", authenticated(s))
	f.with(func(f *fakeVault) {
		h := f.loginHdrs[0]
		if h.Get("X-Vault-Token") != "" || h.Get("X-Vault-Namespace") != "" || h.Get("X-Env") != "" {
			t.Fatalf("login carried the environment: %v", h)
		}
	})
	if s.Client().Token() != "tok-1" || s.Client().Namespace() != "" {
		t.Fatalf("client token %q namespace %q", s.Client().Token(), s.Client().Namespace())
	}
}

func TestCloseStopsTheSession(t *testing.T) {
	f := newFakeVault(t, func(f *fakeVault) { f.live["static"] = true })
	s, err := Login(context.Background(), Config{Address: f.srv.URL, Token: "static"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lookup", authenticated(s))
	s.Close()
	s.Close()
	if err := s.Probe(context.Background()); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("Probe after Close: %v", err)
	}
}

func TestConfigErrors(t *testing.T) {
	id := func(context.Context) (string, error) { return "x", nil }
	for name, cfg := range map[string]Config{
		"no address":         {Token: "t"},
		"no method":          {Address: "http://127.0.0.1:8200"},
		"unknown method":     {Address: "http://127.0.0.1:8200", Method: "ldap"},
		"token without one":  {Address: "http://127.0.0.1:8200", Method: MethodToken},
		"approle no role":    {Address: "http://127.0.0.1:8200", Method: MethodAppRole, SecretID: id},
		"approle no secret":  {Address: "http://127.0.0.1:8200", Method: MethodAppRole, RoleID: "r"},
		"kubernetes no role": {Address: "http://127.0.0.1:8200", Method: MethodKubernetes, KubernetesJWT: id},
		"kubernetes no jwt":  {Address: "http://127.0.0.1:8200", Method: MethodKubernetes, KubernetesRole: "r"},
		"mount with auth/":   {Address: "http://127.0.0.1:8200", Method: MethodAppRole, RoleID: "r", SecretID: id, Mount: "auth/approle"},
	} {
		t.Run(name, func(t *testing.T) {
			if s, err := Login(context.Background(), cfg); err == nil {
				s.Close()
				t.Fatal("accepted")
			}
		})
	}
}

func TestCACertIsTheTrustForATLSVault(t *testing.T) {
	f := &fakeVault{live: map[string]bool{"static": true}, lease: 3600, renewLease: 3600, renewable: true}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})

	s := login(t, Config{Address: f.srv.URL, Token: "static", CACert: ca})
	eventually(t, "lookup over TLS", authenticated(s))
	if err := s.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for name, cfg := range map[string]Config{
		"system roots": {Address: f.srv.URL, Token: "static"},
		"another CA":   {Address: f.srv.URL, Token: "static", CACert: selfSignedCA(t)},
	} {
		t.Run(name, func(t *testing.T) {
			untrusted := login(t, cfg)
			eventually(t, "the certificate refused", func() bool {
				err := untrusted.state()
				return err != nil && strings.Contains(err.Error(), "certificate")
			})
		})
	}
	if _, err := Login(context.Background(), Config{Address: f.srv.URL, Token: "static", CACert: []byte("not a certificate")}); err == nil {
		t.Fatal("a CACert with no PEM certificate was accepted")
	}
}

// selfSignedCA is the PEM of a CA that signed nothing the fake serves.
func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
