//go:build unix

package devproxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func prepareProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func signalProcess(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// proc names a process by PID and start time, so a recycled PID is never signaled.
type proc struct {
	pid   int
	start string
}

// descendants lists every process below root, including those that moved to
// another process group, which a group signal cannot reach. It reads /proc and
// is empty where that is unavailable.
func descendants(root int) []proc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]proc{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, start, _, ok := procStat(pid); ok {
			children[ppid] = append(children[ppid], proc{pid, start})
		}
	}
	var out []proc
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, c := range children[queue[0]] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
	}
	return out
}

func procStat(pid int) (ppid int, start, state string, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", "", false
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, "", "", false
	}
	f := strings.Fields(string(data[i+1:]))
	if len(f) < 20 {
		return 0, "", "", false
	}
	ppid, err = strconv.Atoi(f[1])
	return ppid, f[19], f[0], err == nil
}

func procAlive(p proc) bool {
	_, start, state, ok := procStat(p.pid)
	return ok && start == p.start && state != "Z"
}

func killProcs(ps []proc) {
	for _, p := range ps {
		if procAlive(p) {
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
	}
}

func processGroupAlive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	err := syscall.Kill(-cmd.Process.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func controlListener(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("devproxy: control socket path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("devproxy: refusing to replace non-socket %s", path)
		}
		conn, e := net.DialTimeout("unix", path, 200*time.Millisecond)
		if e == nil {
			_ = conn.Close()
			return nil, errors.New("devproxy: control socket is already in use")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) && !errors.Is(e, os.ErrNotExist) {
			return nil, fmt.Errorf("devproxy: cannot verify stale control socket: %w", e)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}
