// Package devproxy keeps a stable development HTTP listener while complete,
// healthy subprocess generations replace one another behind it.
package devproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Listen, Control                         string
	ReadyTimeout, DrainTimeout, StopTimeout time.Duration
	Output                                  io.Writer
}

// Deployment is submitted over the private Unix socket, never over the public
// proxy. Companions run the same immutable executable with their own arguments.
type Deployment struct {
	Executable  string     `json:"executable"`
	Args        []string   `json:"args"`
	Companions  [][]string `json:"companions,omitempty"`
	Directory   string     `json:"directory"`
	Environment []string   `json:"environment"`
}

type Result struct {
	Generation uint64 `json:"generation"`
	Port       int    `json:"port"`
}

type Status struct {
	PID        int    `json:"pid"`
	Generation uint64 `json:"generation"`
	Port       int    `json:"port"`
	Healthy    bool   `json:"healthy"`
}

type supervisor struct {
	cfg      Config
	ctx      context.Context
	deployMu sync.Mutex
	mu       sync.Mutex // protects active selection and additions to its request group
	active   *generation
	all      map[*generation]bool
	next     uint64
}

type generation struct {
	result     Result
	dir        string
	proxy      *httputil.ReverseProxy
	children   []*child
	requests   sync.WaitGroup
	failed     atomic.Bool
	stopOnce   sync.Once
	readyToken string
}

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func defaults(c Config) Config {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:2052"
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 60 * time.Second
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 30 * time.Second
	}
	if c.StopTimeout <= 0 {
		// HTTP may take 20s to drain, followed by bounded worker/resource
		// cleanup. Leave enough time for interrupted jobs to be acknowledged.
		c.StopTimeout = 45 * time.Second
	}
	if c.Output == nil {
		c.Output = os.Stderr
	}
	c.Output = &lockedWriter{writer: c.Output}
	return c
}

// Run serves until cancellation. No executable is launched until an authorized
// local process submits a deployment. A failed candidate leaves active intact.
func Run(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg = defaults(cfg)
	if cfg.Control == "" {
		return errors.New("devproxy: --control is required")
	}
	public, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer public.Close()
	control, err := controlListener(cfg.Control)
	if err != nil {
		return err
	}
	defer control.Close()
	defer os.Remove(cfg.Control)
	s := &supervisor{cfg: cfg, ctx: ctx, all: make(map[*generation]bool)}
	publicServer := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	controlServer := &http.Server{Handler: s.controlHandler(), ReadHeaderTimeout: 5 * time.Second}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- publicServer.Serve(public) }()
	go func() { errorsCh <- controlServer.Serve(control) }()
	fmt.Fprintf(cfg.Output, "devproxy listening on %s; control %s\n", public.Addr(), cfg.Control)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	cancel()
	// Cancel a preparing deployment before closing generations. Its cleanup is
	// serialized with deployMu so no candidate can be promoted during shutdown.
	_ = controlServer.Close()
	s.deployMu.Lock()
	s.mu.Lock()
	active := s.active
	s.active = nil
	all := make([]*generation, 0, len(s.all))
	for g := range s.all {
		all = append(all, g)
	}
	s.mu.Unlock()
	s.deployMu.Unlock()
	if active != nil {
		s.drain(active)
	}
	for _, g := range all {
		s.stop(g)
	}
	_ = publicServer.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	g := s.active
	if g != nil {
		g.requests.Add(1)
	}
	s.mu.Unlock()
	if g == nil {
		http.Error(w, "development server has no healthy generation yet", http.StatusServiceUnavailable)
		return
	}
	defer g.requests.Done()
	if r.URL.Path == "/readyz" && g.failed.Load() {
		http.Error(w, "development generation lost a process", http.StatusServiceUnavailable)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

func (s *supervisor) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		var d Deployment
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		result, err := s.deploy(r.Context(), d)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		status := Status{PID: os.Getpid()}
		if g := s.active; g != nil {
			status = Status{PID: os.Getpid(), Generation: g.result.Generation, Port: g.result.Port, Healthy: !g.failed.Load()}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	return mux
}

func (s *supervisor) deploy(ctx context.Context, d Deployment) (Result, error) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ReadyTimeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	s.next++
	g, err := s.launch(d, s.next)
	if err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	s.all[g] = true
	s.mu.Unlock()
	promoted := false
	defer func() {
		if !promoted {
			s.stop(g)
		}
	}()
	if err := waitReady(ctx, g); err != nil {
		return Result{}, fmt.Errorf("candidate %d not ready: %w", g.result.Generation, err)
	}
	s.mu.Lock()
	if ctx.Err() != nil || g.failed.Load() {
		s.mu.Unlock()
		return Result{}, errors.New("candidate stopped before promotion")
	}
	old := s.active
	s.active = g
	promoted = true
	s.mu.Unlock()
	fmt.Fprintf(s.cfg.Output, "devproxy promoted generation %d on port %d\n", g.result.Generation, g.result.Port)
	if old != nil {
		go func() { s.drain(old); s.stop(old) }()
	}
	return g.result, nil
}

func (s *supervisor) launch(d Deployment, id uint64) (_ *generation, err error) {
	if !filepath.IsAbs(d.Executable) || !filepath.IsAbs(d.Directory) {
		return nil, errors.New("executable and working directory must be absolute")
	}
	if info, e := os.Stat(d.Directory); e != nil || !info.IsDir() {
		return nil, errors.New("working directory is unavailable")
	}
	source, err := os.Open(d.Executable)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, errors.New("candidate must be a regular executable")
	}
	dir, err := os.MkdirTemp("", "devproxy-generation-")
	if err != nil {
		return nil, err
	}
	g := &generation{dir: dir, result: Result{Generation: id}}
	defer func() {
		if err != nil {
			s.stop(g)
		}
	}()
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return nil, err
	}
	g.readyToken = fmt.Sprintf("%x", token)
	bin := filepath.Join(dir, "server")
	copy, err := os.OpenFile(bin, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(copy, source)
	closeErr := copy.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	commands := append([][]string{d.Args}, d.Companions...)
	for i, args := range commands {
		port, e := unusedPort()
		if e != nil {
			return nil, e
		}
		cmd := exec.Command(bin, args...)
		cmd.Dir = d.Directory
		cmd.Env = processEnvironment(d.Environment, port, g.readyToken)
		cmd.Stdout, cmd.Stderr = s.cfg.Output, s.cfg.Output
		prepareProcess(cmd)
		if err = cmd.Start(); err != nil {
			return nil, err
		}
		c := &child{cmd: cmd, done: make(chan struct{})}
		g.children = append(g.children, c)
		go func() {
			err := c.cmd.Wait()
			g.failed.Store(true)
			close(c.done)
			fmt.Fprintf(s.cfg.Output, "devproxy generation %d process exited: %v\n", id, err)
		}()
		if i == 0 {
			g.result.Port = port
		}
	}
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(g.result.Port))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Every request is forwarded once. The server cannot prove an unsafe
	// request was unobserved, so the transport never retries a reused socket.
	transport.DisableKeepAlives = true
	g.proxy = &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target); r.Out.Host = r.In.Host; r.SetXForwarded() },
		Transport: transport,
		ErrorLog:  log.New(s.cfg.Output, "devproxy: ", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "development upstream unavailable", http.StatusBadGateway)
		},
	}
	return g, nil
}

func waitReady(ctx context.Context, g *generation) error {
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	for {
		if g.failed.Load() {
			return errors.New("a candidate process exited")
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(g.result.Port)+"/readyz", nil)
		req.Header.Set("X-Devproxy-Ready-Token", g.readyToken)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			matched := subtle.ConstantTimeCompare([]byte(resp.Header.Get("X-Devproxy-Ready-Token")), []byte(g.readyToken)) == 1
			if resp.StatusCode == http.StatusOK && matched && !g.failed.Load() {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *supervisor) drain(g *generation) {
	done := make(chan struct{})
	go func() { g.requests.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.cfg.DrainTimeout):
	}
}

func (s *supervisor) stop(g *generation) {
	g.stopOnce.Do(func() {
		for _, c := range g.children {
			signalProcess(c.cmd, false)
		}
		deadline := time.Now().Add(s.cfg.StopTimeout)
		for {
			alive := false
			for _, c := range g.children {
				alive = processGroupAlive(c.cmd) || alive
			}
			if !alive {
				break
			}
			if time.Now().After(deadline) {
				// A leader may already have exited while a grandchild ignores
				// TERM. Kill the groups, not only still-running direct children.
				for _, c := range g.children {
					signalProcess(c.cmd, true)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		for _, c := range g.children {
			<-c.done
		}
		_ = os.RemoveAll(g.dir)
		s.mu.Lock()
		delete(s.all, g)
		s.mu.Unlock()
	})
}

func unusedPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	return port, l.Close()
}

func processEnvironment(env []string, port int, token string) []string {
	out := make([]string, 0, len(env)+2)
	for _, v := range env {
		if !strings.HasPrefix(v, "PORT=") && !strings.HasPrefix(v, "OPS_PORT=") && !strings.HasPrefix(v, "DEVPROXY_READY_TOKEN=") {
			out = append(out, v)
		}
	}
	return append(out, "PORT="+strconv.Itoa(port), "OPS_PORT=0", "DEVPROXY_READY_TOKEN="+token)
}

// Deploy submits one candidate to the private supervisor and waits until it
// has either become active or failed. It never owns the launched processes.
func Deploy(ctx context.Context, control string, d Deployment) (Result, error) {
	var body strings.Builder
	if err := json.NewEncoder(&body).Encode(d); err != nil {
		return Result{}, err
	}
	client := controlClient(control)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://devproxy/deploy", strings.NewReader(body.String()))
	if err != nil {
		return Result{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, errors.New(strings.TrimSpace(string(data)))
	}
	var result Result
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result, err
}

func Inspect(ctx context.Context, control string) (Status, error) {
	client := controlClient(control)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://devproxy/status", nil)
	if err != nil {
		return Status{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	var result Status
	err = json.NewDecoder(resp.Body).Decode(&result)
	return result, err
}

func controlClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}
