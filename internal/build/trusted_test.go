package build

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"codexos/internal/guest"
)

func TestBuildSourceSnapshotUsesFixedToolsAndPublishesArtifacts(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	files = append(files, guest.SnapshotFile{
		Path:    "seed/evil;touch-file.c",
		Content: []byte("int value = 7;\n$(touch " + marker + ")\n"),
	})
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	result := BuildSourceSnapshot(context.Background(), snapshot, output, config)
	if result.Status != BuildStatusSuccess {
		t.Fatalf("build status = %s, diagnostics = %s", result.Status, result.Diagnostics)
	}
	if result.KernelELF == "" || result.ISO == "" {
		t.Fatalf("successful result omitted artifacts: %#v", result)
	}
	kernel, err := os.ReadFile(result.KernelELF)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"-std=c11",
		"-ffreestanding",
		"-fno-stack-protector",
		"-mcmodel=kernel",
		"evil;touch-file.c",
	} {
		if !bytes.Contains(kernel, []byte(expected)) {
			t.Fatalf("kernel does not contain fixed compiler argument %q: %q", expected, kernel)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("guest source influenced host command execution, marker error = %v", err)
	}
	iso, err := os.ReadFile(result.ISO)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(iso, []byte("synthetic-iso\nlimine-installed\n")) {
		t.Fatalf("ISO = %q, want deterministic synthetic image", iso)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "codexos.iso" || entries[1].Name() != "kernel.elf" {
		t.Fatalf("output entries = %#v, want exactly kernel.elf and codexos.iso", entries)
	}
}

func TestBuildSourceSnapshotDoesNotOverwriteExistingArtifacts(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "output")
	first := BuildSourceSnapshot(context.Background(), snapshot, output, config)
	if first.Status != BuildStatusSuccess {
		t.Fatalf("first build status = %s, diagnostics = %s", first.Status, first.Diagnostics)
	}
	wantKernel, err := os.ReadFile(first.KernelELF)
	if err != nil {
		t.Fatal(err)
	}
	wantISO, err := os.ReadFile(first.ISO)
	if err != nil {
		t.Fatal(err)
	}
	second := BuildSourceSnapshot(context.Background(), snapshot, output, config)
	if second.Status != BuildStatusHarnessFailure || !strings.Contains(second.Diagnostics, "already exist") {
		t.Fatalf("second build = %#v, want non-destructive collision failure", second)
	}
	gotKernel, err := os.ReadFile(first.KernelELF)
	if err != nil {
		t.Fatal(err)
	}
	gotISO, err := os.ReadFile(first.ISO)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotKernel, wantKernel) || !bytes.Equal(gotISO, wantISO) {
		t.Fatal("existing artifacts changed after collision")
	}
}

func TestBuildSourceSnapshotBoundsGuestDiagnostics(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	config.DiagnosticLimit = defaultDiagnosticLimit * 2
	compiler := config.Tools.CrossCompiler
	if err := os.WriteFile(compiler, []byte(`#!/bin/sh
case "$1" in
  -print-prog-name=cc1) printf '%s\n' '`+filepath.Join(filepath.Dir(compiler), "cc1")+`'; exit 0 ;;
  -print-prog-name=as) printf '%s\n' '`+filepath.Join(filepath.Dir(compiler), "as")+`'; exit 0 ;;
esac
i=0
while [ "$i" -lt 70000 ]; do printf x >&2; i=$((i + 1)); done
exit 7
`), 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "output")
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	result := BuildSourceSnapshot(context.Background(), snapshot, output, config)
	if result.Status != BuildStatusBuildFailure {
		t.Fatalf("build status = %s, diagnostics = %s", result.Status, result.Diagnostics)
	}
	if len(result.Diagnostics) > defaultDiagnosticLimit {
		t.Fatalf("diagnostics length = %d, want at most %d", len(result.Diagnostics), defaultDiagnosticLimit)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed build created output directory: %v", err)
	}
}

func TestBuildSourceSnapshotCancellationIsBounded(t *testing.T) {
	config, files := syntheticBuildFixture(t)
	config.StepTimeout = 5 * time.Second
	pidPath := filepath.Join(t.TempDir(), "compiler.pid")
	config.Tools.CC = writeExecutable(t, "#!/bin/sh\nprintf '%s' \"$$\" > '"+pidPath+"'\nsleep 30\n")
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resultChannel := make(chan BuildResult, 1)
	go func() {
		resultChannel <- BuildSourceSnapshot(ctx, snapshot, filepath.Join(t.TempDir(), "output"), config)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic compiler did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	var result BuildResult
	select {
	case result = <-resultChannel:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled build did not return")
	}
	if result.Status != BuildStatusHarnessFailure || !strings.Contains(result.Diagnostics, "cancel") {
		t.Fatalf("cancelled build = %#v, want harness cancellation", result)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("compiler process %d survived cancellation: %v", pid, err)
	}
}

func syntheticBuildFixture(t *testing.T) (Config, []guest.SnapshotFile) {
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

	toolchain := filepath.Join(repository, "toolchain")
	bin := filepath.Join(toolchain, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cc1 := filepath.Join(bin, "cc1")
	assembler := filepath.Join(bin, "as")
	writeExecutable(t, "#!/bin/sh\nexit 0\n", cc1)
	writeExecutable(t, "#!/bin/sh\nexit 0\n", assembler)
	crossCompiler := filepath.Join(bin, "x86_64-elf-gcc")
	writeExecutable(t, `#!/bin/sh
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
if [ -z "$output" ]; then echo missing-output >&2; exit 9; fi
printf '%s\n' "$@" > "$output"
`, crossCompiler)
	crossLinker := filepath.Join(bin, "x86_64-elf-ld")
	writeExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then echo missing-output >&2; exit 9; fi
printf 'synthetic-kernel\n' > "$output"
for argument in "$@"; do
  case "$argument" in
    *.o) cat "$argument" >> "$output" ;;
  esac
done
`, crossLinker)
	ldd := writeExecutable(t, "#!/bin/sh\nexit 0\n")
	xorriso := writeExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then echo missing-output >&2; exit 9; fi
printf 'synthetic-iso\n' > "$output"
`)
	cc := writeExecutable(t, `#!/bin/sh
output=
previous=
for argument in "$@"; do
  if [ "$previous" = "-o" ]; then output="$argument"; break; fi
  previous="$argument"
done
if [ -z "$output" ]; then echo missing-output >&2; exit 9; fi
{
  printf '%s\n' '#!/bin/sh'
  printf '%s\n' 'if [ "$1" != "bios-install" ] || [ "$#" -ne 2 ]; then exit 17; fi'
  printf '%s\n' 'printf "%s\\n" "limine-installed" >> "$2"'
} > "$output"
chmod 755 "$output"
`)
	bwrap := writeExecutable(t, `#!/bin/bash
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
`)

	files := []guest.SnapshotFile{
		{Path: "seed/files.h", Content: []byte("struct embedded_file { int unused; };\n")},
		{Path: "seed/linker.ld", Content: []byte("SECTIONS {}\n")},
		{Path: "seed/limine.conf", Content: []byte("TIMEOUT=0\n")},
		{Path: "seed/kernel.c", Content: []byte("int kernel(void) { return 0; }\n")},
	}
	return Config{
		RepositoryRoot: repository,
		Tools: ToolPaths{
			Bwrap:         bwrap,
			CC:            cc,
			LDD:           ldd,
			CrossCompiler: crossCompiler,
			CrossLinker:   crossLinker,
			Xorriso:       xorriso,
		},
	}, files
}

func writeExecutable(t *testing.T, contents string, paths ...string) string {
	t.Helper()
	path := ""
	if len(paths) == 1 {
		path = paths[0]
	} else {
		path = filepath.Join(t.TempDir(), "helper")
	}
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEmbeddedSourcesPreserveFileBytes(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("host C compiler unavailable")
	}
	files := []guest.SnapshotFile{
		{Path: "seed/aλ.c", Content: []byte("λ\x00\n")},
		{Path: "seed/empty", Content: nil},
		{Path: "seed/z.bin", Content: []byte{0, 255, 'z'}},
	}
	// Exercise the generated table through the seed's actual C declaration.
	// The consumer emits each length-delimited path/content with a separator,
	// exposing byte loss, incorrect lengths, ordering, and empty-file handling.
	const consumer = `
#include <stdio.h>
int main(void) {
    if (initial_file_count != 3) return 1;
    for (uint32_t i = 0; i < initial_file_count; ++i) {
        const struct embedded_file *file = &initial_files[i];
        fwrite(file->path, 1, file->path_length, stdout);
        putchar(0);
        fwrite(file->data, 1, file->end - file->data, stdout);
        putchar(0);
    }
    return ferror(stdout) ? 1 : 0;
}
`
	root := t.TempDir()
	source := filepath.Join(root, "embedded.c")
	program := filepath.Join(root, "embedded")
	unsorted := []guest.SnapshotFile{files[2], files[0], files[1]}
	if err := os.WriteFile(source, []byte(renderEmbeddedSources(unsorted)+consumer), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, cc, "-std=c11", "-I", filepath.Join(buildRepositoryRoot(t), "seed"), source, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile embedded sources: %v\n%s", err, output)
	}
	got, err := exec.CommandContext(ctx, program).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, file := range files {
		want = append(want, []byte(file.Path)...)
		want = append(want, 0)
		want = append(want, file.Content...)
		want = append(want, 0)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("embedded file bytes = %q, want %q", got, want)
	}
}

func buildRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate build test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
}
