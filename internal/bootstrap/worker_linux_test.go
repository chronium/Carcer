//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--bootstrap-client-cwd-fixture" {
		var request wireRequest
		if e := readWire(os.Stdin, &request, maxWireInput); e != nil {
			os.Exit(2)
		}
		cwd, e := os.Getwd()
		if e != nil {
			os.Exit(3)
		}
		if e = writeWire(os.Stdout, wireResponse{Result: Result{Status: 0, Reason: "available", Cleaned: true, Diagnostics: cwd}}); e != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "__") {
		if e := Helper(os.Args[1:], os.Stdout); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "--bootstrap-worker-fixture" {
		e := ServeWorker(context.Background(), os.Stdin, os.Stdout, WorkerOptions{os.Args[2], os.Args[3]})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestCollectorRejectsLinksAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("host data"), 0600)
	_ = os.WriteFile(filepath.Join(root, "good"), []byte("opaque bytes"), 0600)
	_ = os.Symlink(outside, filepath.Join(root, "symlink"))
	_ = os.Link(outside, filepath.Join(root, "hardlink"))
	_ = os.Symlink(filepath.Dir(outside), filepath.Join(root, "dirlink"))
	if e := exec.Command("mkfifo", filepath.Join(root, "fifo")).Run(); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	for _, p := range []string{"symlink", "hardlink", "dirlink/secret", "fifo", "../secret", "/etc/passwd", "good/../good", "good//x"} {
		if _, e := CollectFiles(f, []string{p}); e == nil {
			t.Fatalf("accepted unsafe %s", p)
		}
	}
	values, e := CollectFiles(f, []string{"good"})
	if e != nil || string(values[0].Data) != "opaque bytes" {
		t.Fatalf("valid capture %+v %v", values, e)
	}
	large, e := os.Create(filepath.Join(root, "large"))
	if e != nil {
		t.Fatal(e)
	}
	e = large.Truncate(MaxOutput + 1)
	large.Close()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = CollectFiles(f, []string{"large"}); e == nil {
		t.Fatal("oversized output accepted")
	}
}
func TestDiagnosticAndWireBounds(t *testing.T) {
	cancelled := false
	b := boundedBuffer{limit: 8, cancel: func() { cancelled = true }}
	_, _ = b.Write([]byte("12345678"))
	if cancelled {
		t.Fatal("exact boundary rejected")
	}
	_, _ = b.Write([]byte("9"))
	if !cancelled || string(b.Bytes()) != "12345678" {
		t.Fatal("diagnostic bound not enforced")
	}
	var wire bytes.Buffer
	if e := writeWire(&wire, wireRequest{Kind: "probe"}); e != nil {
		t.Fatal(e)
	}
	var req wireRequest
	if e := readWire(&wire, &req, 8); e == nil {
		t.Fatal("oversized wire input accepted")
	}
	if e := readWire(strings.NewReader("\x00\x00\x00\x08{}"), &req, 16); e != io.ErrUnexpectedEOF {
		t.Fatalf("partial publication input: %v", e)
	}
}
func acceptanceClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("CODEXOS_BOOTSTRAP_ACCEPTANCE") != "1" {
		t.Skip("set CODEXOS_BOOTSTRAP_ACCEPTANCE=1 for serialized rootless Podman acceptance")
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	slice := fmt.Sprintf("bootstrapaccept%d.slice", os.Getpid())
	anchor := fmt.Sprintf("bootstrapaccept%d.service", os.Getpid())
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(args[0], args[1:]...)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%v: %v: %s", args, e, b)
		}
	}
	run("systemd-run", "--user", "--unit="+anchor, "--slice="+slice, "--property=RuntimeMaxSec=1200", "/usr/bin/sleep", "1200")
	run("systemctl", "--user", "set-property", "--runtime", slice, "MemoryMax=805306368", "MemorySwapMax=0", "CPUQuota=100%", "TasksMax=96")
	dir := filepath.Join(t.TempDir(), "worker")
	t.Cleanup(func() {
		// The worker owns cleanup. Audit before removing the disposable aggregate.
		b, e := exec.Command("podman", "ps", "-aq", "--filter", "label=io.codexos.bootstrap=1").Output()
		if e != nil || len(bytes.TrimSpace(b)) != 0 {
			t.Errorf("leftover bootstrap containers: %s %v", b, e)
		}
		_ = exec.Command("systemctl", "--user", "stop", anchor, slice).Run()
		_ = exec.Command("systemctl", "--user", "revert", slice).Run()
	})
	return &Client{Command: []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--slice=" + slice, exe, "--bootstrap-worker-fixture", dir, slice}}
}
func TestRootlessWorkerSmoke(t *testing.T) {
	c := acceptanceClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	v, e := c.call(ctx, wireRequest{Kind: "probe"})
	if e != nil || v.Result.Status != 0 {
		t.Fatalf("preflight: %+v %v", v, e)
	}
	req := fixtureRequest()
	req.Argv = []string{"/bin/sh", "-ec", "test $(id -u) = 65534; test $(awk '/^CapEff:/ {print $2}' /proc/self/status) = 0000000000000000; test $(awk '/^NoNewPrivs:/ {print $2}' /proc/self/status) = 1; test $(awk '/^Seccomp:/ {print $2}' /proc/self/status) = 2; test $(ls /sys/class/net) = lo; test ! -w /control; if kill -0 1 2>/dev/null; then exit 9; fi; printf opaque > /work/out/tool"}
	v, e = c.call(ctx, wireRequest{Kind: "job", Request: req, Snapshot: fixtureSnapshot(t), TCCAsset: "tcc"})
	if e != nil || v.Result.Status != 0 || !v.Result.Cleaned || len(v.Outputs) != 1 || string(v.Outputs[0].Data) != "opaque" {
		t.Fatalf("job: %+v %v", v, e)
	}
	t.Logf("worker smoke: reason=%s cleaned=%t output=%s", v.Result.Reason, v.Result.Cleaned, v.Outputs[0].Data)
}

func TestRootlessRecoversInterruptedWorker(t *testing.T) {
	c := acceptanceClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req := fixtureRequest()
	req.Argv = []string{"/bin/sh", "-c", "sleep 300"}
	req.Outputs = nil
	done := make(chan error, 1)
	go func() {
		_, e := c.call(ctx, wireRequest{Kind: "job", Request: req, Snapshot: fixtureSnapshot(t), TCCAsset: "tcc"})
		done <- e
	}()
	dir := c.Command[len(c.Command)-2]
	var active map[string]string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if readJSON(filepath.Join(dir, "active.json"), &active, 4096) == nil {
			st, e := inspect(ctx, active["container"])
			if e == nil && st.State.Pid > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	pid, e := strconv.Atoi(active["worker_pid"])
	if e != nil || pid <= 1 {
		t.Fatalf("worker did not publish owned PID: %v", active)
	}
	process, e := os.FindProcess(pid)
	if e != nil {
		t.Fatal(e)
	}
	if e = process.Kill(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
		if e == nil {
			t.Fatal("interrupted worker claimed success")
		}
	case <-ctx.Done():
		t.Fatal("interrupted worker did not retire")
	}
	response, e := c.call(ctx, wireRequest{Kind: "recover"})
	if e != nil || response.Result.Status != 0 || !response.Result.Cleaned {
		t.Fatalf("recovery %+v %v", response, e)
	}
	if _, e = os.Stat(filepath.Join(dir, "active.json")); !os.IsNotExist(e) {
		t.Fatal("recovery retained active marker")
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "inputs-") {
			t.Fatal("interrupted input capture retained")
		}
	}
	t.Log("abrupt worker death: owned container, descendants, staged inputs and active marker recovered")
}
