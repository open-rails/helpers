package devproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real subprocess HTTP server, copied into each generation by launch.
func TestProxyProcess(t *testing.T) {
	if os.Getenv("DEVPROXY_TEST_PROCESS") != "1" {
		return
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "grandchild" {
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "companion-exit" {
		os.Exit(17)
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "companion-dies-later" {
		time.Sleep(200 * time.Millisecond)
		os.Exit(18)
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "companion" {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		<-c
		os.Exit(0)
	}
	name := os.Getenv("DEVPROXY_TEST_NAME")
	if marker := os.Getenv("DEVPROXY_TEST_GRANDCHILD"); marker != "" {
		self, _ := os.Executable()
		cmd := exec.Command(self, "-test.run=^TestProxyProcess$", "--", "grandchild")
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte(fmt.Sprint(cmd.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	readyAt := time.Now()
	if raw := os.Getenv("DEVPROXY_TEST_DELAY"); raw != "" {
		d, _ := time.ParseDuration(raw)
		readyAt = readyAt.Add(d)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(readyAt) || os.Getenv("DEVPROXY_TEST_BAD") == "1" {
			w.WriteHeader(503)
			return
		}
		if r.Header.Get("X-Devproxy-Ready-Token") == os.Getenv("DEVPROXY_READY_TOKEN") && os.Getenv("DEVPROXY_TEST_WRONG_TOKEN") == "" {
			w.Header().Set("X-Devproxy-Ready-Token", os.Getenv("DEVPROXY_READY_TOKEN"))
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, name)
	})
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = rw.Flush()
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			fmt.Fprintf(rw, "%s:%s", name, line)
			if rw.Flush() != nil {
				return
			}
		}
	})
	mux.HandleFunc("/drop", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.OpenFile(os.Getenv("DEVPROXY_TEST_REQUESTS"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(f, r.Method)
		_ = f.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "%s|%s|%s", name, r.Host, r.Method) })
	server := &http.Server{Addr: "127.0.0.1:" + os.Getenv("PORT"), Handler: mux}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	_ = server.ListenAndServe()
	os.Exit(0)
}

type rig struct {
	ctx           context.Context
	cancel        context.CancelFunc
	control, base string
	done          chan error
	t             *testing.T
}

func newRig(t *testing.T, ready, drain time.Duration) *rig {
	t.Helper()
	port, err := unusedPort()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{ctx: ctx, cancel: cancel, control: filepath.Join(dir, "control.sock"), base: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan error, 1), t: t}
	go func() {
		r.done <- Run(ctx, Config{Listen: fmt.Sprintf("127.0.0.1:%d", port), Control: r.control, ReadyTimeout: ready, DrainTimeout: drain, StopTimeout: time.Second, Output: io.Discard})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if status, err := Inspect(ctx, r.control); err == nil {
			if status.PID != os.Getpid() {
				t.Fatalf("controller PID = %d, want %d", status.PID, os.Getpid())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := os.Stat(r.control)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("control mode: %v %v", info, err)
	}
	return r
}

func (r *rig) deployment(name string, extra ...string) Deployment {
	r.t.Helper()
	bin, err := os.Executable()
	if err != nil {
		r.t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		r.t.Fatal(err)
	}
	env := append(os.Environ(), "DEVPROXY_TEST_PROCESS=1", "DEVPROXY_TEST_NAME="+name, "GORACE=atexit_sleep_ms=0")
	env = append(env, extra...)
	return Deployment{Executable: bin, Args: []string{"-test.run=^TestProxyProcess$"}, Directory: cwd, Environment: env}
}
func (r *rig) get(path string) string {
	r.t.Helper()
	resp, err := http.Get(r.base + path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func TestFailedReadinessKeepsLastHealthyAndPreservesHost(t *testing.T) {
	r := newRig(t, 400*time.Millisecond, time.Second)
	first, err := Deploy(r.ctx, r.control, r.deployment("first"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, r.base+"/", strings.NewReader("one-shot"))
	req.Host = "hentai.local:3002"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "first|hentai.local:3002|POST" {
		t.Fatalf("host/method: %s", body)
	}
	if _, err := Deploy(r.ctx, r.control, r.deployment("bad", "DEVPROXY_TEST_BAD=1")); err == nil {
		t.Fatal("unhealthy candidate promoted")
	}
	status, err := Inspect(r.ctx, r.control)
	if err != nil || !status.Healthy || status.Generation != first.Generation {
		t.Fatalf("last healthy lost: %+v %v", status, err)
	}
	if got := r.get("/"); !strings.HasPrefix(got, "first|") {
		t.Fatal(got)
	}
	if _, err := Deploy(r.ctx, r.control, r.deployment("wrong-listener", "DEVPROXY_TEST_WRONG_TOKEN=1")); err == nil {
		t.Fatal("200 from a listener without this generation's token was accepted")
	}
	bad := r.deployment("companion-bad")
	bad.Companions = [][]string{{"-test.run=^TestProxyProcess$", "--", "companion-exit"}}
	if _, err := Deploy(r.ctx, r.control, bad); err == nil {
		t.Fatal("dead companion promoted")
	}
	if got := r.get("/"); !strings.HasPrefix(got, "first|") {
		t.Fatal(got)
	}
}

func TestPromotionDrainsConcurrentRequestAndSerializesCandidates(t *testing.T) {
	r := newRig(t, 2*time.Second, 2*time.Second)
	if _, err := Deploy(r.ctx, r.control, r.deployment("old")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(r.base + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	d := r.deployment("new", "DEVPROXY_TEST_DELAY=150ms")
	d.Companions = [][]string{{"-test.run=^TestProxyProcess$", "--", "companion"}}
	firstDone := make(chan error, 1)
	go func() { _, err := Deploy(r.ctx, r.control, d); firstDone <- err }()
	time.Sleep(30 * time.Millisecond)
	for range 5 {
		if got := r.get("/"); !strings.HasPrefix(got, "old|") {
			t.Fatal("old unavailable while candidate starts:", got)
		}
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := r.get("/"); !strings.HasPrefix(got, "new|") {
		t.Fatal(got)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "old" {
		t.Fatalf("old request interrupted: %q %v", b, err)
	}
	var results [2]Result
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			var err error
			results[i], err = Deploy(r.ctx, r.control, r.deployment(fmt.Sprint("parallel-", i)))
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if results[0].Generation == results[1].Generation {
		t.Fatal("candidates shared a generation")
	}
	status, err := Inspect(r.ctx, r.control)
	if err != nil || !status.Healthy {
		t.Fatalf("parallel status: %+v %v", status, err)
	}
}

func TestUpgradeStaysOnOldGenerationUntilDrainDeadline(t *testing.T) {
	r := newRig(t, time.Second, 350*time.Millisecond)
	if _, err := Deploy(r.ctx, r.control, r.deployment("old")); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(r.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: socket.local\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("upgrade: %s %v", line, err)
	}
	for {
		line, err = rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := Deploy(r.ctx, r.control, r.deployment("new")); err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(conn, "still-open\n")
	line, err = rd.ReadString('\n')
	if err != nil || line != "old:still-open\n" {
		t.Fatalf("upgraded request moved/interrupted: %q %v", line, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = rd.ReadString('\n')
	if err == nil {
		t.Fatal("old upgraded connection exceeded drain deadline")
	}
	if got := r.get("/"); !strings.HasPrefix(got, "new|") {
		t.Fatal(got)
	}
}

func TestUnsafeRequestIsNotRetriedAndBinaryIsImmutable(t *testing.T) {
	r := newRig(t, time.Second, time.Second)
	d := r.deployment("original", "DEVPROXY_TEST_REQUESTS="+filepath.Join(t.TempDir(), "requests"))
	requests := strings.TrimPrefix(d.Environment[len(d.Environment)-1], "DEVPROXY_TEST_REQUESTS=")
	source, err := os.Open(d.Executable)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "candidate")
	copy, err := os.OpenFile(copyPath, os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(copy, source)
	source.Close()
	copy.Close()
	if err != nil {
		t.Fatal(err)
	}
	d.Executable = copyPath
	if _, err := Deploy(r.ctx, r.control, d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, []byte("a broken replacement build"), 0700); err != nil {
		t.Fatal("supervisor retained the mutable source executable:", err)
	}
	resp, err := http.Post(r.base+"/drop", "text/plain", strings.NewReader("charge-once"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatal(resp.Status)
	}
	count, err := os.ReadFile(requests)
	if err != nil || string(count) != "POST\n" {
		t.Fatalf("request replayed: %q %v", count, err)
	}
	if _, err := Deploy(r.ctx, r.control, d); err == nil {
		t.Fatal("broken executable promoted")
	}
	if got := r.get("/"); !strings.HasPrefix(got, "original|") {
		t.Fatal("failed replacement affected active binary:", got)
	}
}

func TestCompanionExitMarksOnlyItsGenerationDegraded(t *testing.T) {
	r := newRig(t, time.Second, time.Second)
	d := r.deployment("companion-owner")
	d.Companions = [][]string{{"-test.run=^TestProxyProcess$", "--", "companion-dies-later"}}
	if _, err := Deploy(r.ctx, r.control, d); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := Inspect(r.ctx, r.control)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Healthy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dead companion left generation healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Get(r.base + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.Status)
	}
	if _, err := Deploy(r.ctx, r.control, r.deployment("replacement")); err != nil {
		t.Fatal(err)
	}
	if got := r.get("/"); !strings.HasPrefix(got, "replacement|") {
		t.Fatal(got)
	}
	status, err := Inspect(r.ctx, r.control)
	if err != nil || !status.Healthy {
		t.Fatalf("old failure poisoned replacement: %+v %v", status, err)
	}
}

func TestRetirementKillsDescendantAfterLeaderExits(t *testing.T) {
	r := newRig(t, time.Second, time.Second)
	marker := filepath.Join(t.TempDir(), "grandchild")
	if _, err := Deploy(r.ctx, r.control, r.deployment("old", "DEVPROXY_TEST_GRANDCHILD="+marker)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if _, err := Deploy(r.ctx, r.control, r.deployment("new")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || (err == nil && strings.Contains(string(data), ") Z ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TERM-ignoring descendant %d survived its leader", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCanceledQueuedDeploymentDoesNotReplaceHealthyCandidate(t *testing.T) {
	r := newRig(t, time.Second, time.Second)
	if _, err := Deploy(r.ctx, r.control, r.deployment("first")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Deploy(r.ctx, r.control, r.deployment("slow", "DEVPROXY_TEST_DELAY=200ms"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(r.ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := Deploy(ctx, r.control, r.deployment("canceled")); err == nil {
		t.Fatal("canceled deployment succeeded")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.get("/"); !strings.HasPrefix(got, "slow|") {
		t.Fatal("canceled candidate superseded a healthy deployment:", got)
	}
}
