//go:build unix

package devproxy

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
