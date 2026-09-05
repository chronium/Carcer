//go:build linux

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type completion struct {
	Nonce string `json:"nonce"`
	Exit  int    `json:"exit"`
}
type supervisorRequest struct {
	Nonce string   `json:"nonce"`
	Argv  []string `json:"argv"`
}

// Supervise is PID 1 in the container, UID 0 with only SETUID/SETGID. The
// workload runs as 65534 with no capabilities and cannot write/read /control
// or signal/ptrace this different-UID supervisor. stdout is never completion.
func Supervise() error {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("bootstrap supervisor requires container PID 1/UID 0")
	}
	var r supervisorRequest
	if e := readJSON("/inputs/control.json", &r, MaxRequest); e != nil {
		return e
	}
	if !validID(r.Nonce) || len(r.Argv) == 0 {
		return errors.New("invalid supervisor input")
	}
	if e := os.MkdirAll("/work/out", 0755); e != nil {
		return e
	}
	if e := os.Chmod("/work/out", 0777); e != nil {
		return e
	}
	c := exec.Command(r.Argv[0], r.Argv[1:]...)
	c.Dir = "/work"
	c.Env = []string{"HOME=/work", "TMPDIR=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C"}
	c.Stdin = nil
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}, Pdeathsig: syscall.SIGKILL}
	code := 0
	if e := c.Run(); e != nil {
		code = 127
		var x *exec.ExitError
		if errors.As(e, &x) {
			code = x.ExitCode()
		}
		fmt.Fprintln(os.Stderr, e)
	}
	if e := atomicJSON("/control/result.json", completion{r.Nonce, code}); e != nil {
		return e
	}
	// Hold the mount namespace until trusted capture and whole-cgroup teardown.
	for {
		time.Sleep(time.Hour)
	}
}

func processStart(pid int) (string, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return "", e
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return "", errors.New("invalid process identity")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", errors.New("short process identity")
	}
	return f[19], nil
}
func processRoot(pid int, start string) (*os.File, int, error) {
	fd, e := unix.PidfdOpen(pid, 0)
	if e != nil {
		return nil, -1, e
	}
	fail := func(e error) (*os.File, int, error) { unix.Close(fd); return nil, -1, e }
	got, e := processStart(pid)
	if e != nil || got != start {
		return fail(errors.New("container process identity changed"))
	}
	// This one magic link is host-selected and bound to a live pidfd/start time.
	// All subsequent path resolution is anchored to this pinned directory FD.
	root, e := os.Open(fmt.Sprintf("/proc/%d/root", pid))
	if e != nil {
		return fail(e)
	}
	got, e = processStart(pid)
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, pe := unix.Poll(poll, 0)
	if e != nil || pe != nil || got != start || poll[0].Revents != 0 {
		root.Close()
		return fail(errors.New("container process retired during root capture"))
	}
	return root, fd, nil
}
func openBeneath(root *os.File, p string, directory bool) (*os.File, error) {
	if !safePath(p) {
		return nil, errors.New("unsafe collection path")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, e := unix.Openat2(int(root.Fd()), p, &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), p), nil
}
func regularBytes(root *os.File, p string, limit int64) ([]byte, error) {
	f, e := openBeneath(root, p, false)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || !ok || sys.Nlink != 1 || st.Size() < 0 || st.Size() > limit {
		return nil, errors.New("output must be a bounded regular file with one link")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) != st.Size() {
		return nil, errors.New("output changed or exceeded size bound")
	}
	after, e := f.Stat()
	if e != nil || !os.SameFile(st, after) || after.Size() != st.Size() || after.ModTime() != st.ModTime() {
		return nil, errors.New("output changed during capture")
	}
	return b, nil
}

// CollectFiles is also tested directly against hostile fixture paths. The
// caller freezes the whole container before invoking this bounded collector.
func CollectFiles(root *os.File, names []string) ([]Input, error) {
	if len(names) > 32 {
		return nil, errors.New("output count limit")
	}
	seen := map[string]bool{}
	var total int
	out := make([]Input, 0, len(names))
	for _, name := range names {
		if seen[name] || !safePath(name) {
			return nil, errors.New("unsafe/duplicate output path")
		}
		seen[name] = true
		b, e := regularBytes(root, name, MaxOutput)
		if e != nil {
			return nil, fmt.Errorf("output %q: %w", name, e)
		}
		total += len(b)
		if total > MaxOutputs {
			return nil, errors.New("aggregate output bound exceeded")
		}
		out = append(out, Input{name, b})
	}
	return out, nil
}

// Helper is reachable only as an explicit fixed internal command. It never
// executes an argument as a host command, and has no privilege elevation.
func Helper(args []string, output io.Writer) error {
	if len(args) < 1 {
		return errors.New("missing helper mode")
	}
	if args[0] == "__supervise" && len(args) == 1 {
		return Supervise()
	}
	if len(args) != 4 {
		return errors.New("invalid collector arguments")
	}
	pid, e := strconv.Atoi(args[1])
	if e != nil || pid <= 1 {
		return errors.New("invalid container PID")
	}
	root, pidfd, e := processRoot(pid, args[2])
	if e != nil {
		return e
	}
	defer root.Close()
	defer unix.Close(pidfd)
	switch args[0] {
	case "__status":
		b, e := regularBytes(root, "control/result.json", 1024)
		if e != nil {
			return e
		}
		var done completion
		if e = strictJSON(b, &done, 1024); e != nil {
			return e
		}
		if done.Nonce != args[3] {
			return errors.New("completion nonce mismatch")
		}
		return json.NewEncoder(output).Encode(done)
	case "__collect":
		var names []string
		if e = strictJSON([]byte(args[3]), &names, MaxRequest); e != nil {
			return e
		}
		dir, e := openBeneath(root, "work/out", true)
		if e != nil {
			return e
		}
		defer dir.Close()
		values, e := CollectFiles(dir, names)
		if e != nil {
			return e
		}
		return json.NewEncoder(output).Encode(values)
	default:
		return errors.New("unknown helper command")
	}
}

// command is reserved for trusted fixed programs and internally constructed
// arguments. Guest argv is passed solely through supervisorRequest inside Podman.
func command(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = 2 * time.Second
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process != nil {
			return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	var b boundedBuffer
	b.limit = limit
	b.cancel = func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	}
	c.Stdout = &b
	c.Stderr = &b
	e := c.Run()
	if b.overflow {
		return b.Bytes(), errors.New("trusted command output limit")
	}
	return b.Bytes(), e
}
func executablePath() (string, error) {
	p, e := os.Executable()
	if e != nil {
		return "", e
	}
	return filepath.EvalSymlinks(p)
}
