//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"codexos/internal/sourcecapacity"
)

const JobSlice = "codexosbootstrap.slice"
const maxWireInput = (MaxInputs+sourcecapacity.Expanded+sourcecapacity.FramingOverhead)*4/3 + 128*1024
const maxWireOutput = MaxOutputs*4/3 + 1024*1024

type wireRequest struct {
	Kind     string                `json:"kind"`
	Request  Request               `json:"request"`
	Snapshot []byte                `json:"snapshot"`
	Budget   sourcecapacity.Budget `json:"budget"`
	Inputs   []Input               `json:"inputs"`
	TCCAsset string                `json:"tcc_asset"`
}
type wireResponse struct {
	Result  Result  `json:"result"`
	Outputs []Input `json:"outputs"`
}

func writeWire(w io.Writer, v any) error {
	if response, ok := v.(wireResponse); ok {
		response.Result.Diagnostics = diagnosticText(response.Result.Diagnostics)
		v = response
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > maxWireInput {
		return errors.New("worker message exceeds bound")
	}
	if e = binary.Write(w, binary.BigEndian, uint32(len(b))); e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func readWire(r io.Reader, v any, limit int) error {
	var n uint32
	if e := binary.Read(r, binary.BigEndian, &n); e != nil {
		return e
	}
	if int64(n) > int64(limit) {
		return errors.New("worker message exceeds bound")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(r, b); e != nil {
		return e
	}
	return strictJSON(b, v, limit)
}

type boundedBuffer struct {
	mu       sync.Mutex
	b        bytes.Buffer
	limit    int
	overflow bool
	cancel   func()
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := b.limit - b.b.Len()
	if len(p) > left {
		p = p[:left]
		if !b.overflow {
			b.overflow = true
			if b.cancel != nil {
				b.cancel()
			}
		}
	}
	_, _ = b.b.Write(p)
	return n, nil
}
func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.b.Bytes()...)
}
func (b *boundedBuffer) Overflow() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.overflow }

// WorkerOptions are trusted process setup, never part of the wire request.
// The installed entry point fixes Directory and Slice for the dedicated account.
type WorkerOptions struct {
	Directory string
	Slice     string
}
type containerState struct {
	Mounts []struct {
		Type        string
		Source      string
		Destination string
		RW          bool
	}
	HostConfig struct {
		NetworkMode    string
		ReadonlyRootfs bool
		Privileged     bool
		PidMode        string
		UTSMode        string
	}
	State struct {
		Status     string
		Pid        int
		ConmonPid  int
		Paused     bool
		Running    bool
		OOMKilled  bool
		ExitCode   int
		CgroupPath string
	}
}

func podman(ctx context.Context, args ...string) ([]byte, error) {
	b, err := command(ctx, MaxDiagnostics, "/usr/bin/podman", args...)
	if err != nil {
		return b, fmt.Errorf("Podman: %w: %s", err, b)
	}
	return b, nil
}
func inspect(ctx context.Context, name string) (containerState, error) {
	var v []containerState
	b, e := podman(ctx, "inspect", name)
	if e != nil {
		return containerState{}, e
	}
	if e = json.Unmarshal(b, &v); e != nil || len(v) != 1 {
		return containerState{}, errors.New("invalid Podman inspection")
	}
	return v[0], nil
}
func preflight(ctx context.Context, o WorkerOptions) error {
	b, e := podman(ctx, "info", "--format=json")
	if e != nil {
		return e
	}
	var info struct {
		Host struct {
			CgroupVersion string
			CgroupManager string
			Security      struct {
				Rootless       bool
				SeccompEnabled bool
				SelinuxEnabled bool
			}
		}
	}
	if json.Unmarshal(b, &info) != nil || !info.Host.Security.Rootless || !info.Host.Security.SeccompEnabled || !info.Host.Security.SelinuxEnabled || info.Host.CgroupVersion != "v2" || info.Host.CgroupManager != "systemd" {
		return errors.New("required rootless controls unavailable")
	}
	b, e = podman(ctx, "image", "inspect", Image, "--format", "{{.Id}} {{.Os}} {{.Architecture}}")
	if e != nil {
		return fmt.Errorf("pinned image unavailable (jobs never pull): %w", e)
	}
	if strings.TrimSpace(string(b)) != ImageID+" linux amd64" {
		return errors.New("bootstrap image/platform identity mismatch")
	}
	if runtime.GOARCH != "amd64" {
		return errors.New("bootstrap image requires Linux amd64")
	}
	enforcing, e := os.ReadFile("/sys/fs/selinux/enforce")
	if e != nil || strings.TrimSpace(string(enforcing)) != "1" {
		return errors.New("bootstrap requires enforcing SELinux")
	}
	b, e = command(ctx, 4096, "/usr/bin/systemctl", "--user", "show", o.Slice, "--property=ControlGroup", "--value")
	if e != nil {
		return e
	}
	parent := strings.TrimSpace(string(b))
	if parent == "" || !strings.HasPrefix(parent, "/") {
		return errors.New("bootstrap aggregate slice unavailable")
	}
	own, e := os.ReadFile("/proc/self/cgroup")
	if e != nil || !strings.Contains(string(own), parent+"/") {
		return errors.New("bootstrap worker must itself run inside the aggregate slice")
	}
	for name, want := range map[string]string{"memory.max": "805306368", "memory.swap.max": "0", "cpu.max": "100000 100000", "pids.max": "96"} {
		b, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", parent, name))
		if e != nil || strings.TrimSpace(string(b)) != want {
			return fmt.Errorf("aggregate %s must be %s", name, want)
		}
	}
	return nil
}
func cleanupContainer(name string, known containerState) error {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	b, e := podman(ctx, "rm", "--force", "--time=2", "--ignore", name)
	if e != nil {
		return fmt.Errorf("container cleanup failed: %w: %s", e, b)
	}
	c := exec.CommandContext(ctx, "/usr/bin/podman", "container", "exists", name)
	e = c.Run()
	var x *exec.ExitError
	if !errors.As(e, &x) || x.ExitCode() != 1 {
		return errors.New("container absence was not confirmed")
	}
	if p := known.State.CgroupPath; p != "" {
		b, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", p, "cgroup.events"))
		if e == nil && strings.Contains(string(b), "populated 1") {
			return errors.New("container descendants remain in cgroup")
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	return nil
}
func recoverContainers(ctx context.Context, o WorkerOptions) error {
	b, e := podman(ctx, "ps", "-aq", "--filter", "label=io.codexos.bootstrap=1")
	if e != nil {
		return e
	}
	ids := strings.Fields(string(b))
	if len(ids) > 1 {
		return errors.New("unexpected multiple bootstrap containers; operator cleanup required")
	}
	for _, id := range ids {
		st, e := inspect(ctx, id)
		if e != nil {
			return e
		}
		if e = cleanupContainer(id, st); e != nil {
			return e
		}
	}
	entries, e := os.ReadDir(o.Directory)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "inputs-") {
			if e := os.RemoveAll(filepath.Join(o.Directory, entry.Name())); e != nil {
				return e
			}
		}
	}
	e = os.Remove(filepath.Join(o.Directory, "active.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	return e
}

// ServeWorker handles one bounded request and exits. Keeping stdin open is the
// caller's lease: EOF or any extra control byte cancels execution independently
// of an outer Go operation lock. The per-job runtime deadline remains mandatory.
func ServeWorker(ctx context.Context, input io.ReadCloser, output io.Writer, o WorkerOptions) error {
	if os.Geteuid() == 0 {
		return errors.New("bootstrap worker must be rootless")
	}
	if e := secureDirectory(o.Directory); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(o.Directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return writeWire(output, wireResponse{Result: Result{Status: 2, Reason: "busy", Cleaned: true}})
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()
	// Interrupt a caller that stalls halfway through even the initial message.
	stopRead := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stopRead()
	var request wireRequest
	if e = readWire(input, &request, maxWireInput); e != nil {
		return e
	}
	done := make(chan struct{})
	go func() { var b [1]byte; _, _ = input.Read(b[:]); cancel(); close(done) }()
	defer func() { _ = input.Close(); <-done }()
	response := wireResponse{Result: Result{Status: 2, Reason: "runtime_failure"}}
	if e = recoverContainers(ctx, o); e != nil {
		response.Result.Diagnostics = e.Error()
		return writeWire(output, response)
	}
	response.Result.Cleaned = true
	if e = preflight(ctx, o); e != nil {
		response.Result.Diagnostics = e.Error()
		return writeWire(output, response)
	}
	switch request.Kind {
	case "probe", "recover":
		response.Result.Status = 0
		response.Result.Reason = "available"
		return writeWire(output, response)
	case "job":
		response = executeJob(ctx, request, o)
		return writeWire(output, response)
	default:
		response.Result.Reason = "invalid_worker_request"
		return writeWire(output, response)
	}
}
func stageInputs(root string, w wireRequest) error {
	r := w.Request
	if e := r.Validate(); e != nil {
		return e
	}
	_, snap, e := ParseRequest(mustJSON(r), w.Snapshot, w.Budget)
	if e != nil {
		return e
	}
	if !safeAssetID(w.TCCAsset) {
		return errors.New("missing pinned source identity")
	}
	expected := map[string]string{}
	for _, a := range r.Assets {
		if a.ID == w.TCCAsset && a.SHA256 != TCCSHA256 {
			return errors.New("upstream TCC pin mismatch")
		}
		expected["assets/"+a.ID] = a.SHA256
	}
	for _, a := range r.Artifacts {
		expected["artifacts/"+a] = a
	}
	if len(w.Inputs) != len(expected) {
		return errors.New("captured input count mismatch")
	}
	write := func(p string, b []byte) error {
		p = filepath.Join(root, p)
		if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return e
		}
		return os.WriteFile(p, b, 0444)
	}
	total := 0
	for _, in := range w.Inputs {
		digest, ok := expected[in.Path]
		if !ok || !safePath(in.Path) || Digest(in.Data) != digest {
			return errors.New("captured input identity mismatch")
		}
		delete(expected, in.Path)
		total += len(in.Data)
		if total > MaxInputs {
			return errors.New("captured input quota")
		}
		if e = write(in.Path, in.Data); e != nil {
			return e
		}
	}
	for _, f := range snap.Files() {
		if e = write("source/"+f.Path, f.Content); e != nil {
			return e
		}
	}
	return nil
}

func executeJob(parent context.Context, w wireRequest, o WorkerOptions) (response wireResponse) {
	response.Result = Result{Status: 2, Reason: "runtime_failure", Cleaned: true, ExitCode: -1}
	inputs, e := os.MkdirTemp(o.Directory, "inputs-")
	if e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	defer os.RemoveAll(inputs)
	if e = os.Chmod(inputs, 0755); e == nil {
		e = stageInputs(inputs, w)
	}
	if e != nil {
		response.Result.Reason = "invalid_inputs"
		response.Result.Diagnostics = e.Error()
		return
	}
	nonce := randomID()
	name := "codexos-bootstrap-" + nonce[:24]
	if e = os.WriteFile(filepath.Join(inputs, "control.json"), mustJSON(supervisorRequest{nonce, w.Request.Argv}), 0444); e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	helper, e := executablePath()
	if e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	// Both the immutable executable and staged inputs are private copies on the
	// worker account. Never relabel the installed binary or a harness directory.
	helperBytes, e := sourcecapacity.ReadFile(helper, 16<<20)
	if e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	if e = os.Mkdir(filepath.Join(inputs, "hooks"), 0700); e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	helperCopy := filepath.Join(inputs, "helper")
	if e = os.WriteFile(helperCopy, helperBytes, 0555); e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	response.Result.WorkerSHA256 = Digest(helperBytes)
	helperBytes = nil
	w.Snapshot = nil
	w.Inputs = nil
	runtime.GC()
	ctx, cancel := context.WithTimeout(parent, 180*time.Second)
	defer cancel()
	args := []string{"create", "--name", name, "--label=io.codexos.bootstrap=1", "--pull=never", "--cgroup-parent=" + o.Slice,
		"--network=none", "--pid=private", "--uts=private", "--cgroupns=private", "--uidmap=0:0:65536", "--gidmap=0:0:65536", "--image-volume=ignore", "--hooks-dir=" + filepath.Join(inputs, "hooks"), "--read-only", "--read-only-tmpfs=false", "--ipc=none", "--cap-drop=all", "--cap-add=SETUID,SETGID", "--security-opt=no-new-privileges", "--user=0:0",
		"--tmpfs=/work:rw,nosuid,nodev,exec,size=256m,mode=1777", "--tmpfs=/tmp:rw,nosuid,nodev,exec,size=16m,mode=1777", "--tmpfs=/control:rw,nosuid,nodev,noexec,size=1m,mode=0700",
		"--pids-limit=64", "--memory=512m", "--memory-swap=512m", "--cpus=1", "--ulimit=nofile=128:128", "--ulimit=core=0:0", "--ulimit=fsize=67108864:67108864",
		"--log-driver=none", "--http-proxy=false", "--unsetenv-all", "--env=HOME=/work", "--env=PATH=/usr/local/bin:/usr/bin:/bin", "--env=LANG=C", "--env=GOMAXPROCS=2", "--env=GOMEMLIMIT=64MiB",
		"--workdir=/work", "--timeout=180", "--stop-timeout=2", "-v", inputs + ":/inputs:ro,Z", Image, "/inputs/helper", "__supervise"}
	if e = atomicJSON(filepath.Join(o.Directory, "active.json"), map[string]string{"container": name, "worker_pid": strconv.Itoa(os.Getpid())}); e != nil {
		response.Result.Diagnostics = e.Error()
		return
	}
	var state containerState
	var attach *exec.Cmd
	var attached chan error
	response.Result.Cleaned = false
	defer func() {
		cleanErr := cleanupContainer(name, state)
		if attach != nil && attached != nil {
			select {
			case <-attached:
			case <-time.After(3 * time.Second):
				_ = attach.Process.Kill()
				<-attached
				cleanErr = errors.Join(cleanErr, errors.New("container attachment failed to retire"))
			}
		}
		if cleanErr != nil {
			response.Result.Status = 2
			response.Result.Reason = "cleanup_failed"
			response.Result.Diagnostics += "\n" + cleanErr.Error()
			response.Outputs = nil
			response.Result.Artifacts = nil
			return
		}
		response.Result.Cleaned = true
		_ = os.Remove(filepath.Join(o.Directory, "active.json"))
	}()
	if b, e := podman(ctx, args...); e != nil {
		response.Result.Diagnostics = string(b) + e.Error()
		return
	}
	created, err := inspect(ctx, name)
	if err != nil {
		response.Result.Diagnostics = err.Error()
		return
	}
	if !created.HostConfig.ReadonlyRootfs || created.HostConfig.Privileged || created.HostConfig.NetworkMode != "none" || created.HostConfig.PidMode == "host" || created.HostConfig.UTSMode == "host" || len(created.Mounts) != 1 {
		response.Result.Diagnostics = "container isolation configuration mismatch"
		return
	}
	mount := created.Mounts[0]
	if mount.Type != "bind" || mount.Source != inputs || mount.Destination != "/inputs" || mount.RW {
		response.Result.Diagnostics = "unexpected container host mount"
		return
	}
	logs := &boundedBuffer{limit: MaxDiagnostics, cancel: cancel}
	attach = exec.Command("/usr/bin/podman", "start", "-a", name)
	attach.WaitDelay = 2 * time.Second
	attach.Stdout = logs
	attach.Stderr = logs
	if e = attach.Start(); e != nil {
		response.Result.Diagnostics = e.Error()
		attach = nil
		return
	}
	attached = make(chan error, 1)
	go func() { attached <- attach.Wait() }()
	defer func() {
		response.Result.Diagnostics = string(logs.Bytes()) + response.Result.Diagnostics
		if len(response.Result.Diagnostics) > MaxDiagnostics {
			response.Result.Diagnostics = response.Result.Diagnostics[:MaxDiagnostics]
		}
	}()
	var start, jobCgroup string
	for ctx.Err() == nil {
		state, e = inspect(ctx, name)
		if e != nil {
			response.Result.Diagnostics = e.Error()
			break
		}
		if state.State.Pid == 0 && (state.State.Status == "created" || state.State.Status == "configured") {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if state.State.Pid == 0 {
			if state.State.OOMKilled {
				response.Result.OOM = true
				response.Result.Reason = "oom"
			} else {
				response.Result.Reason = "container_exit"
			}
			response.Result.Status = 1
			response.Result.ExitCode = state.State.ExitCode
			return
		}
		if start == "" {
			var controls map[string]string
			jobCgroup, controls, e = verifyJobControls(state.State.Pid, o.Slice)
			if e != nil {
				response.Result.Diagnostics = e.Error()
				return
			}
			response.Result.Controls = controls
			start, e = processStart(state.State.Pid)
			if e != nil {
				response.Result.Diagnostics = e.Error()
				break
			}
		}
		// Result is read from root-owned /control, never from the diagnostic stream.
		b, se := command(ctx, 4096, "/usr/bin/podman", "unshare", helper, "__status", strconv.Itoa(state.State.Pid), start, nonce)
		if se == nil {
			var done completion
			if e = strictJSON(b, &done, 4096); e != nil || done.Nonce != nonce {
				response.Result.Diagnostics = "invalid trusted completion"
				return
			}
			response.Result.ExitCode = done.Exit
			response.Result.ResourceEvents = resourceEvents(jobCgroup)
			response.Result.OOM = response.Result.ResourceEvents["oom_kill"] > 0
			if done.Exit != 0 {
				response.Result.Status = 1
				response.Result.Reason = "exit"
				if response.Result.OOM {
					response.Result.Reason = "oom"
				} else if response.Result.ResourceEvents["pids_max"] > 0 {
					response.Result.Reason = "pids_limit"
				}
				return
			}
			if _, e = podman(ctx, "pause", name); e != nil {
				response.Result.Diagnostics = e.Error()
				return
			}
			paused, e := inspect(ctx, name)
			if e != nil || !paused.State.Paused || paused.State.Pid != state.State.Pid {
				response.Result.Diagnostics = "container freeze/identity check failed"
				return
			}
			freeze, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", jobCgroup, "cgroup.events"))
			if e != nil || !strings.Contains(string(freeze), "frozen 1") {
				response.Result.Diagnostics = "kernel cgroup freeze was not confirmed"
				return
			}
			captureCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			b, e = command(captureCtx, maxWireOutput, "/usr/bin/podman", "unshare", helper, "__collect", strconv.Itoa(state.State.Pid), start, string(mustJSON(w.Request.Outputs)))
			stop()
			if e != nil {
				response.Result.Status = 1
				response.Result.Reason = "unsafe_output"
				response.Result.Diagnostics = string(b) + e.Error()
				return
			}
			if e = strictJSON(b, &response.Outputs, maxWireOutput); e != nil {
				response.Result.Diagnostics = e.Error()
				return
			}
			for _, out := range response.Outputs {
				response.Result.Artifacts = append(response.Result.Artifacts, Artifact{Digest(out.Data), out.Path, int64(len(out.Data))})
			}
			response.Result.Status = 0
			response.Result.Reason = "completed"
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	response.Result.Status = 1
	switch {
	case logs.Overflow():
		response.Result.Reason = "diagnostic_limit"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		response.Result.Reason = "deadline"
	case ctx.Err() != nil:
		response.Result.Reason = "cancelled"
	default:
		response.Result.Status = 2
		response.Result.Reason = "runtime_failure"
	}
	return
}

func verifyJobControls(pid int, slice string) (string, map[string]string, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if e != nil {
		return "", nil, e
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "0::/") || strings.Contains(line, "\n") {
		return "", nil, errors.New("invalid job cgroup membership")
	}
	group := strings.TrimPrefix(line, "0::")
	if !strings.Contains(group, "/"+slice+"/") {
		return "", nil, errors.New("job escaped aggregate slice")
	}
	got := map[string]string{}
	for name, want := range map[string]string{"memory.max": "536870912", "memory.swap.max": "0", "cpu.max": "100000 100000", "pids.max": "64"} {
		b, e = os.ReadFile(filepath.Join("/sys/fs/cgroup", group, name))
		if e != nil {
			return "", nil, e
		}
		value := strings.TrimSpace(string(b))
		if value != want {
			return "", nil, fmt.Errorf("job %s=%s, expected %s", name, value, want)
		}
		got[name] = value
	}
	return group, got, nil
}
func resourceEvents(group string) map[string]uint64 {
	out := map[string]uint64{}
	for _, file := range []string{"memory.events", "pids.events"} {
		b, e := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, file))
		if e != nil {
			continue
		}
		fields := strings.Fields(string(b))
		for i := 0; i+1 < len(fields); i += 2 {
			key := fields[i]
			if file == "pids.events" {
				key = "pids_" + key
			}
			n, e := strconv.ParseUint(fields[i+1], 10, 64)
			if e == nil {
				out[key] = n
			}
		}
	}
	return out
}
