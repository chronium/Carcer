package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"codexos/internal/agent"
	"codexos/internal/build"
	"codexos/internal/experiment"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/qemu"
)

const disposableGenerationHandoff = "The disposable generation completed its validated build."

func TestRunnerCompletesDisposableGenerationThroughAgentAndBuild(t *testing.T) {
	t.Setenv("CODEXOS_DISPOSABLE_QEMU_LIFECYCLE", "lifecycle")
	processRecords := t.TempDir()
	t.Setenv("CODEXOS_DISPOSABLE_PROCESS_RECORDS", processRecords)
	qemuExecutable := buildDisposableRunnerQEMU(t)
	codexExecutable := buildDisposableOperatorFixture(t, "fake-codex", "./internal/operator/testdata/fakecodex")
	buildConfig := disposableTrustedBuildConfig(t)
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Keep the root short: the candidate workspace adds two random path
	// components and Linux Unix-domain socket paths are limited to 108 bytes.
	runDirectory, err := os.MkdirTemp("/tmp", "co-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDirectory) })
	initialISO := filepath.Join(t.TempDir(), "initial.iso")
	if err := os.WriteFile(initialISO, []byte("disposable initial image"), 0o600); err != nil {
		t.Fatal(err)
	}

	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer inputWriter.Close()
	var output synchronizedBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runWithIOConfigured(ctx, Options{
			RunDirectory: runDirectory,
			InitialISO:   initialISO,
		}, input, &output, runnerConfiguration{
			live: experiment.LiveRunOptions{
				QEMUExecutable:        qemuExecutable,
				HardwareProfile:       qemu.TestHardwareProfile,
				BuildConfig:           buildConfig,
				ReadyTimeout:          3 * time.Second,
				CandidateReadyTimeout: 3 * time.Second,
			},
			session: agent.GenerationSessionOptions{
				Executable:  codexExecutable,
				AuthFile:    authFile,
				StopTimeout: 3 * time.Second,
			},
		})
	}()
	if _, err := io.WriteString(inputWriter, "agent\n"); err != nil {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("send agent command: %v; stop runner: %v", err, stopErr)
	}

	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var stoppedEarly bool
	var runnerErr error
	var completionTimedOut bool
	for !strings.Contains(output.String(), "Generation 0 completed cooperatively.") && !stoppedEarly && !completionTimedOut {
		select {
		case err := <-result:
			stoppedEarly = true
			runnerErr = err
		case <-poll.C:
		case <-deadline.C:
			completionTimedOut = true
		}
	}
	if stoppedEarly || completionTimedOut {
		if !stoppedEarly {
			runnerErr = stopDisposableRunner(cancel, inputWriter, result)
		}
		if stoppedEarly {
			t.Fatalf("runner stopped before reaching the completed gate: %v\n%s", runnerErr, output.String())
		}
		t.Fatalf("runner did not reach the completed gate: %v\n%s", runnerErr, output.String())
	}
	if _, err := io.WriteString(inputWriter, "quit\n"); err != nil {
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("send quit command: %v; stop runner: %v", err, stopErr)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run disposable completed generation: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		stopErr := stopDisposableRunner(cancel, inputWriter, result)
		t.Fatalf("runner did not stop after quit: %v\n%s", stopErr, output.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("runner exceeded acceptance deadline: %v", ctx.Err())
	}
	assertDisposableProcessesStopped(t, processRecords)

	for _, want := range []string{
		"Codex planning and implementation started for generation 0.",
		"Generation 0 completed cooperatively.",
		"A successor is selected.",
		disposableGenerationHandoff,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("operator output missing %q:\n%s", want, output.String())
		}
	}
	loaded, err := experiment.NewCodexOSRun(runDirectory)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := loaded.InspectGeneration(0)
	if err != nil {
		t.Fatal(err)
	}
	if archive.Outcome != "completed" || archive.Transition != "initial" || archive.ParentGeneration != nil || archive.Handoff == nil || *archive.Handoff != disposableGenerationHandoff {
		t.Fatalf("completed archive = %#v", archive)
	}
	bootISO, err := os.ReadFile(filepath.Join(archive.ArchivePath, "boot", "codexos.iso"))
	if err != nil || string(bootISO) != "disposable initial image" {
		t.Fatalf("archived boot ISO = %q, %v", bootISO, err)
	}
	snapshot, err := os.ReadFile(filepath.Join(archive.ArchivePath, "source.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := guest.DecodeSourceSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("archived source files = %#v", files)
	}
	for _, file := range files {
		materialized, err := os.ReadFile(filepath.Join(archive.ArchivePath, "source", filepath.FromSlash(file.Path)))
		if err != nil || !bytes.Equal(materialized, file.Content) {
			t.Fatalf("materialized source %s differs from snapshot: %v", file.Path, err)
		}
	}
	successorISO, err := os.ReadFile(filepath.Join(archive.ArchivePath, "successor", "codexos.iso"))
	if err != nil || string(successorISO) != "synthetic-iso\nlimine-installed\n" {
		t.Fatalf("validated successor ISO = %q, %v", successorISO, err)
	}
	for _, path := range []string{
		filepath.Join(archive.ArchivePath, "successor", "kernel.elf"),
		filepath.Join(archive.ArchivePath, "successor", "codexos.iso"),
		filepath.Join(runDirectory, "planning-evidence", "generation-0000", "manifest.json"),
	} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("required completed artifact %s: %v", path, err)
		}
	}
	buildManifests, err := filepath.Glob(filepath.Join(runDirectory, "build-review-provenance", "generation-0000", "build-*", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(buildManifests) != 1 {
		t.Fatalf("build evidence manifests = %v", buildManifests)
	}
	manifest, err := os.ReadFile(buildManifests[0])
	if err != nil {
		t.Fatal(err)
	}
	var buildEvidence map[string]any
	if err := json.Unmarshal(manifest, &buildEvidence); err != nil {
		t.Fatal(err)
	}
	candidate, _ := buildEvidence["candidate_validation"].(map[string]any)
	if buildEvidence["outcome"] != "success" || candidate["protocol_validated"] != true {
		t.Fatalf("build evidence does not prove validated success: %s", manifest)
	}
	workspaces, err := filepath.Glob(filepath.Join(runDirectory, ".generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("runner left generation workspaces: %v", workspaces)
	}
	events, err := os.ReadFile(filepath.Join(runDirectory, observability.EventLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, event := range []string{
		"run_started", "generation_started", "codex_session_started", "planning_started", "planning_completed",
		"build_attempt_received", "build_candidate_validation_started", "build_candidate_qemu_started",
		"build_candidate_ready_observed", "build_protocol_validation_started", "build_protocol_validation_completed",
		"build_attempt_completed", "build_completed", "generation_completed", "codex_session_stopped", "run_stopped",
	} {
		index := bytes.Index(events, []byte(`"event":"`+event+`"`))
		if index < 0 || index <= previous {
			t.Fatalf("event %q missing or out of order:\n%s", event, events)
		}
		previous = index
	}
}

func stopDisposableRunner(cancel context.CancelFunc, input *os.File, result <-chan error) error {
	cancel()
	_ = input.Close()
	select {
	case err := <-result:
		return err
	case <-time.After(15 * time.Second):
		return context.DeadlineExceeded
	}
}

func assertDisposableProcessesStopped(t *testing.T, directory string) {
	t.Helper()
	records, err := filepath.Glob(filepath.Join(directory, "*.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 3 {
		t.Fatalf("disposable process records = %v, want active QEMU, candidate QEMU, and Codex", records)
	}
	for _, record := range records {
		encoded, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(encoded)))
		if err != nil {
			t.Fatalf("invalid disposable PID record %s: %v", record, err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("disposable process %d from %s survived runner shutdown: %v", pid, record, err)
		}
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func buildDisposableOperatorFixture(t *testing.T, name, packagePath string) string {
	t.Helper()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", executable, packagePath)
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build disposable %s fixture: %v\n%s", name, err, output)
	}
	return executable
}

func disposableTrustedBuildConfig(t *testing.T) build.Config {
	t.Helper()
	repository := t.TempDir()
	limineDirectory := filepath.Join(repository, "third_party", "limine")
	if err := os.MkdirAll(limineDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"limine.c", "limine-bios-hdd.h", "limine-bios.sys", "limine-bios-cd.bin"} {
		if err := os.WriteFile(filepath.Join(limineDirectory, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(repository, "toolchain", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cc1 := disposableExecutable(t, "#!/bin/sh\nexit 0\n", filepath.Join(bin, "cc1"))
	assembler := disposableExecutable(t, "#!/bin/sh\nexit 0\n", filepath.Join(bin, "as"))
	crossCompiler := filepath.Join(bin, "x86_64-elf-gcc")
	disposableExecutable(t, `#!/bin/sh
case "$1" in
  -print-prog-name=cc1) printf '%s\n' '`+cc1+`'; exit 0 ;;
  -print-prog-name=as) printf '%s\n' '`+assembler+`'; exit 0 ;;
esac
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf '%s\n' "$@" > "$output"
`, crossCompiler)
	crossLinker := filepath.Join(bin, "x86_64-elf-ld")
	disposableExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf 'synthetic-kernel\n' > "$output"
for argument in "$@"; do
  case "$argument" in *.o) cat "$argument" >> "$output" ;; esac
done
`, crossLinker)
	ldd := disposableExecutable(t, "#!/bin/sh\nexit 0\n", filepath.Join(bin, "ldd"))
	xorriso := disposableExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
printf 'synthetic-iso\n' > "$output"
`, filepath.Join(bin, "xorriso"))
	cc := disposableExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then exit 9; fi
{
  printf '%s\n' '#!/bin/sh'
  printf '%s\n' 'if [ "$1" != "bios-install" ] || [ "$#" -ne 2 ]; then exit 17; fi'
  printf '%s\n' 'printf "%s\\n" "limine-installed" >> "$2"'
} > "$output"
chmod 755 "$output"
`, filepath.Join(bin, "cc"))
	bwrap := disposableExecutable(t, `#!/bin/bash
workspace=
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "--bind" ]]; then workspace="$2"; shift 3; continue; fi
  if [[ "$1" == "--" ]]; then shift; break; fi
  shift
done
mapped=()
for argument in "$@"; do
  if [[ "$argument" == /workspace/* ]]; then
    mapped+=("$workspace${argument#/workspace}")
  else
    mapped+=("$argument")
  fi
done
exec "${mapped[@]}"
`, filepath.Join(bin, "bwrap"))
	return build.Config{
		RepositoryRoot: repository,
		Tools: build.ToolPaths{
			Bwrap: bwrap, CC: cc, LDD: ldd, CrossCompiler: crossCompiler,
			CrossLinker: crossLinker, Xorriso: xorriso,
		},
		StepTimeout: 3 * time.Second,
	}
}

func disposableExecutable(t *testing.T, contents, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
