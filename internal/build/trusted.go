// Package build contains the fixed trusted CodexOS source build operation.
//
// Source snapshots are untrusted input.  They are materialized into a fresh
// workspace and consumed by fixed host tools; no command from a snapshot is
// ever interpreted or executed.
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"codexos/internal/guest"
	"codexos/internal/sourcecapacity"
)

const (
	defaultDiagnosticLimit = 64 * 1024
	defaultStepTimeout     = 60 * time.Second
	processReapTimeout     = time.Second
	trustedPathEnvironment = "/bin:/usr/bin"
)

// BuildStatus is the outcome category returned by BuildSourceSnapshot.
type BuildStatus string

const (
	BuildStatusSuccess        BuildStatus = "success"
	BuildStatusBuildFailure   BuildStatus = "build_failure"
	BuildStatusHarnessFailure BuildStatus = "harness_failure"
)

// BuildResult is the bounded result of one trusted build attempt.  KernelELF
// and ISO are host paths only on success; they are empty otherwise.
type BuildResult struct {
	Status      BuildStatus
	Diagnostics string
	KernelELF   string
	ISO         string
}

// ToolPaths names the six fixed host programs used by the trusted build.
// Empty fields are resolved from PATH.  Values are trusted harness
// configuration, not guest-controlled data.
type ToolPaths struct {
	Bwrap         string
	CC            string
	LDD           string
	CrossCompiler string
	CrossLinker   string
	Xorriso       string
}

// Config controls trusted host inputs.  RepositoryRoot must contain the
// pinned third_party/limine inputs.  Empty RepositoryRoot enables discovery.
// The zero timeout and diagnostic limit select the Python reference defaults.
type Config struct {
	SourceCapacity  sourcecapacity.Budget
	RepositoryRoot  string
	Tools           ToolPaths
	StepTimeout     time.Duration
	DiagnosticLimit int
}

// BuildSourceSnapshot builds one validated source snapshot with fixed trusted
// operations.  A zero Config resolves the repository and utilities from the
// trusted process environment.
func BuildSourceSnapshot(ctx context.Context, snapshotData []byte, outputDirectory string, configuration Config) BuildResult {
	if ctx == nil {
		return BuildResult{Status: BuildStatusHarnessFailure, Diagnostics: "build context is nil"}
	}
	return buildSourceSnapshot(ctx, snapshotData, outputDirectory, configuration)
}

type normalizedConfig struct {
	repositoryRoot  string
	tools           ToolPaths
	stepTimeout     time.Duration
	diagnosticLimit int
	outputDirectory string
}

type runtimeLibrary struct {
	destination string
	source      string
}

type sandboxMounts struct {
	bwrap     string
	toolchain string
	trusted   string
	xorriso   string
	libraries []runtimeLibrary
}

type buildStepError struct {
	program  string
	err      error
	exit     int
	timedOut bool
	canceled bool
}

func (e *buildStepError) Error() string {
	if e.timedOut {
		return "build step timed out: " + e.program
	}
	if e.canceled {
		return "build step cancelled: " + e.program
	}
	if e.exit != 0 {
		return fmt.Sprintf("build step failed with exit code %d: %s", e.exit, e.program)
	}
	if e.err != nil {
		return "could not start build step: " + e.program + ": " + e.err.Error()
	}
	return "build step failed: " + e.program
}

func (e *buildStepError) Unwrap() error { return e.err }

func buildSourceSnapshot(ctx context.Context, snapshotData []byte, outputDirectory string, configuration Config) BuildResult {
	snapshot, err := guest.ParseSourceSnapshotWithBudget(snapshotData, configuration.SourceCapacity)
	if err != nil {
		return harnessFailure(err)
	}
	files := snapshot.Files()
	if err := validateRequiredInputs(files); err != nil {
		return harnessFailure(err)
	}
	config, err := configuration.normalized()
	if err != nil {
		return harnessFailure(err)
	}
	limine, err := limineInputs(config.repositoryRoot)
	if err != nil {
		return harnessFailure(err)
	}
	output, err := resolvePath(outputDirectory)
	if err != nil {
		return harnessFailure(err)
	}
	if sameOrWithin(output, config.repositoryRoot) {
		return harnessFailure(errors.New("build output directory must be outside the repository"))
	}
	config.outputDirectory = output
	if err := ctx.Err(); err != nil {
		return harnessFailure(err)
	}

	temporary, err := os.MkdirTemp("", "codexos-build-")
	if err != nil {
		return harnessFailure(err)
	}
	defer os.RemoveAll(temporary)
	workspace, err := resolvePath(temporary)
	if err != nil {
		return harnessFailure(err)
	}
	if err := materialize(files, workspace); err != nil {
		return harnessFailure(err)
	}
	buildDirectory := filepath.Join(workspace, "build")
	objects := filepath.Join(buildDirectory, "objects")
	isoRoot := filepath.Join(buildDirectory, "iso-root")
	if err := os.MkdirAll(objects, 0o755); err != nil {
		return harnessFailure(err)
	}
	trustedLimine, err := copyLimineInputs(limine, workspace)
	if err != nil {
		return harnessFailure(err)
	}

	diagnosticsPath := filepath.Join(buildDirectory, "diagnostics.log")
	diagnosticsFile, err := os.OpenFile(diagnosticsPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return harnessFailure(err)
	}
	diagnostics := &diagnosticLog{file: diagnosticsFile, limit: config.diagnosticLimit}
	defer diagnostics.Close()

	limineTool := filepath.Join(workspace, "trusted", "limine", "limine")
	if err := runStep(ctx, []string{
		config.tools.CC, "-std=c99", "-O2", trustedLimine["limine.c"], "-o", limineTool,
	}, workspace, diagnostics, config.stepTimeout, trustedPathEnvironment); err != nil {
		return harnessFailureWithDiagnostics(diagnostics, err)
	}
	sandbox, err := prepareSandbox(ctx, config, limineTool, filepath.Join(workspace, "trusted"))
	if err != nil {
		return harnessFailureWithDiagnostics(diagnostics, err)
	}

	objectPaths, err := compileGuestSources(ctx, files, workspace, objects, config.tools.CrossCompiler, sandbox, diagnostics, config.stepTimeout)
	if err != nil {
		return buildFailureOrHarness(diagnostics, err)
	}

	embeddedSource := filepath.Join(buildDirectory, "embedded-sources.c")
	if err := os.WriteFile(embeddedSource, []byte(renderEmbeddedSources(files)), 0o600); err != nil {
		return harnessFailureWithDiagnostics(diagnostics, err)
	}
	embeddedObject := filepath.Join(objects, "embedded-sources.o")
	embeddedCommand := append([]string{config.tools.CrossCompiler}, cFlags...)
	embeddedCommand = append(embeddedCommand, "-I", "/workspace/seed", "-c", sandboxPath(embeddedSource, workspace), "-o", sandboxPath(embeddedObject, workspace))
	if err := runSandboxed(ctx, embeddedCommand, workspace, sandbox, diagnostics, config.stepTimeout); err != nil {
		return buildFailureOrHarness(diagnostics, err)
	}
	objectPaths = append(objectPaths, embeddedObject)

	kernel := filepath.Join(buildDirectory, "kernel.elf")
	linkCommand := append([]string{config.tools.CrossLinker}, linkFlags...)
	linkCommand = append(linkCommand, "-T", "/workspace/seed/linker.ld")
	for _, objectPath := range objectPaths {
		linkCommand = append(linkCommand, sandboxPath(objectPath, workspace))
	}
	linkCommand = append(linkCommand, "-o", sandboxPath(kernel, workspace))
	if err := runSandboxed(ctx, linkCommand, workspace, sandbox, diagnostics, config.stepTimeout); err != nil {
		return buildFailureOrHarness(diagnostics, err)
	}

	iso, err := createISO(ctx, workspace, isoRoot, kernel, limineTool, trustedLimine, sandbox, diagnostics, config.stepTimeout)
	if err != nil {
		return buildFailureOrHarness(diagnostics, err)
	}
	if err := ctx.Err(); err != nil {
		return harnessFailureWithDiagnostics(diagnostics, err)
	}

	diagnosticText := diagnostics.text("")
	if err := os.MkdirAll(config.outputDirectory, 0o755); err != nil {
		return harnessFailure(err)
	}
	finalKernel := filepath.Join(config.outputDirectory, "kernel.elf")
	finalISO := filepath.Join(config.outputDirectory, "codexos.iso")
	if pathExists(finalKernel) || pathExists(finalISO) {
		return harnessFailure(errors.New("build output files already exist"))
	}
	if err := copyFile(kernel, finalKernel); err != nil {
		return harnessFailure(err)
	}
	if err := copyFile(iso, finalISO); err != nil {
		return harnessFailure(err)
	}
	return BuildResult{Status: BuildStatusSuccess, Diagnostics: diagnosticText, KernelELF: finalKernel, ISO: finalISO}
}

var cFlags = []string{
	"-std=c11", "-O2", "-Wall", "-Wextra", "-Werror", "-ffreestanding",
	"-fno-stack-protector", "-fno-pic", "-fno-pie", "-fno-asynchronous-unwind-tables",
	"-m64", "-march=x86-64", "-mno-red-zone", "-mno-mmx", "-mno-sse", "-mno-sse2", "-mcmodel=kernel",
}

var assemblyFlags = append([]string(nil), cFlags[5:]...)

var linkFlags = []string{"-static", "--build-id=none", "-z", "max-page-size=0x1000"}

func (c Config) normalized() (normalizedConfig, error) {
	if c.StepTimeout < 0 {
		return normalizedConfig{}, errors.New("build step timeout must not be negative")
	}
	if c.DiagnosticLimit < 0 {
		return normalizedConfig{}, errors.New("diagnostic limit must not be negative")
	}
	stepTimeout := c.StepTimeout
	if stepTimeout == 0 {
		stepTimeout = defaultStepTimeout
	}
	diagnosticLimit := c.DiagnosticLimit
	if diagnosticLimit == 0 {
		diagnosticLimit = defaultDiagnosticLimit
	} else if diagnosticLimit > defaultDiagnosticLimit {
		// The Python reference exposes a fixed 64 KiB diagnostic bound. Keep
		// trusted build responses bounded even when a caller supplies a larger
		// harness configuration value.
		diagnosticLimit = defaultDiagnosticLimit
	}

	tools := c.Tools
	var err error
	tools.Bwrap, err = findConfiguredExecutable(tools.Bwrap, "bwrap")
	if err != nil {
		return normalizedConfig{}, errors.New("missing required build utility: bwrap")
	}
	tools.CC, err = findConfiguredExecutable(tools.CC, "cc")
	if err != nil {
		return normalizedConfig{}, errors.New("missing required build utility: cc")
	}
	tools.LDD, err = findConfiguredExecutable(tools.LDD, "ldd")
	if err != nil {
		return normalizedConfig{}, errors.New("missing required build utility: ldd")
	}
	tools.CrossCompiler, err = findConfiguredExecutable(tools.CrossCompiler, "x86_64-elf-gcc")
	if err != nil {
		return normalizedConfig{}, errors.New("missing required build utility: x86_64-elf-gcc")
	}
	tools.CrossLinker, err = findConfiguredExecutable(tools.CrossLinker, "x86_64-elf-ld")
	if err != nil {
		return normalizedConfig{}, errors.New("missing required build utility: x86_64-elf-ld")
	}
	tools.Xorriso, err = findConfiguredExecutable(tools.Xorriso, "xorriso")
	if err != nil {
		return normalizedConfig{}, errors.New("missing required build utility: xorriso")
	}

	repository := c.RepositoryRoot
	if repository == "" {
		repository, err = discoverRepositoryRoot()
		if err != nil {
			return normalizedConfig{}, err
		}
	} else {
		repository, err = resolvePath(repository)
		if err != nil {
			return normalizedConfig{}, err
		}
	}
	info, err := os.Stat(repository)
	if err != nil {
		return normalizedConfig{}, err
	}
	if !info.IsDir() {
		return normalizedConfig{}, errors.New("build repository root is not a directory")
	}

	return normalizedConfig{
		repositoryRoot:  repository,
		tools:           tools,
		stepTimeout:     stepTimeout,
		diagnosticLimit: diagnosticLimit,
	}, nil
}

func validateRequiredInputs(files []guest.SnapshotFile) error {
	paths := make(map[string]struct{}, len(files))
	hasSource := false
	for _, file := range files {
		paths[file.Path] = struct{}{}
		if strings.HasSuffix(file.Path, ".c") || strings.HasSuffix(file.Path, ".S") {
			hasSource = true
		}
	}
	for _, required := range []string{"seed/files.h", "seed/linker.ld", "seed/limine.conf"} {
		if _, ok := paths[required]; !ok {
			return fmt.Errorf("missing required build input: %s", required)
		}
	}
	if !hasSource {
		return errors.New("source snapshot contains no C or assembly source")
	}
	return nil
}

func limineInputs(repository string) (map[string]string, error) {
	directory := filepath.Join(repository, "third_party", "limine")
	inputs := make(map[string]string, 4)
	for _, name := range []string{"limine.c", "limine-bios-hdd.h", "limine-bios.sys", "limine-bios-cd.bin"} {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("missing pinned Limine input: %s", name)
		}
		inputs[name] = path
	}
	return inputs, nil
}

func copyLimineInputs(inputs map[string]string, workspace string) (map[string]string, error) {
	directory := filepath.Join(workspace, "trusted", "limine")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(inputs))
	for _, name := range []string{"limine.c", "limine-bios-hdd.h", "limine-bios-cd.bin", "limine-bios.sys"} {
		destination := filepath.Join(directory, name)
		if err := copyFile(inputs[name], destination); err != nil {
			return nil, err
		}
		result[name] = destination
	}
	return result, nil
}

func materialize(files []guest.SnapshotFile, workspace string) error {
	for _, file := range files {
		if err := guest.ValidateSourcePath(file.Path); err != nil {
			return err
		}
		destination, err := resolvePath(filepath.Join(workspace, filepath.FromSlash(file.Path)))
		if err != nil {
			return fmt.Errorf("cannot materialize source path %q: %w", file.Path, err)
		}
		if !sameOrWithin(destination, workspace) {
			return fmt.Errorf("source path escapes build workspace: %s", file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("cannot materialize source path %q: %w", file.Path, err)
		}
		if err := os.WriteFile(destination, file.Content, 0o600); err != nil {
			return fmt.Errorf("cannot materialize source path %q: %w", file.Path, err)
		}
	}
	return nil
}
func prepareSandbox(ctx context.Context, config normalizedConfig, limineTool, trusted string) (sandboxMounts, error) {
	compiler, err := resolvePath(config.tools.CrossCompiler)
	if err != nil {
		return sandboxMounts{}, err
	}
	linker, err := resolvePath(config.tools.CrossLinker)
	if err != nil {
		return sandboxMounts{}, err
	}
	toolchain := filepath.Dir(filepath.Dir(compiler))
	if !sameOrWithin(linker, toolchain) {
		return sandboxMounts{}, errors.New("cross compiler and linker have different prefixes")
	}

	compilerPrograms := make([]string, 0, 2)
	for _, program := range []string{"cc1", "as"} {
		output, err := captureTrusted(ctx, []string{compiler, "-print-prog-name=" + program}, config.stepTimeout)
		if err != nil {
			return sandboxMounts{}, fmt.Errorf("cannot inspect build utility: %s", compiler)
		}
		candidate := strings.TrimSpace(output)
		if candidate == "" {
			return sandboxMounts{}, fmt.Errorf("cross compiler program is outside its prefix: %s", candidate)
		}
		if !filepath.IsAbs(candidate) {
			candidate, err = filepath.Abs(candidate)
			if err != nil {
				return sandboxMounts{}, err
			}
		}
		candidate, err = resolvePath(candidate)
		if err != nil {
			return sandboxMounts{}, err
		}
		info, statErr := os.Stat(candidate)
		if statErr != nil || !info.Mode().IsRegular() || !sameOrWithin(candidate, toolchain) {
			return sandboxMounts{}, fmt.Errorf("cross compiler program is outside its prefix: %s", candidate)
		}
		compilerPrograms = append(compilerPrograms, candidate)
	}

	xorriso, err := resolvePath(config.tools.Xorriso)
	if err != nil {
		return sandboxMounts{}, err
	}
	libraries, err := runtimeLibraries(ctx, config.tools.LDD, append([]string{compiler, linker, xorriso, limineTool}, compilerPrograms...), config.stepTimeout)
	if err != nil {
		return sandboxMounts{}, err
	}
	return sandboxMounts{
		bwrap:     config.tools.Bwrap,
		toolchain: toolchain,
		trusted:   trusted,
		xorriso:   xorriso,
		libraries: libraries,
	}, nil
}

func runtimeLibraries(ctx context.Context, ldd string, executables []string, timeout time.Duration) ([]runtimeLibrary, error) {
	libraries := make(map[string]string)
	for _, executable := range executables {
		output, err := captureTrusted(ctx, []string{ldd, executable}, timeout)
		if err != nil {
			return nil, fmt.Errorf("cannot inspect build utility: %s", ldd)
		}
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] == "linux-vdso.so.1" {
				continue
			}
			var destination string
			if len(fields) >= 3 && fields[1] == "=>" {
				if fields[2] == "not" {
					return nil, fmt.Errorf("missing runtime library for build utility: %s", fields[0])
				}
				destination = fields[2]
			} else if strings.HasPrefix(fields[0], "/") {
				destination = fields[0]
			} else {
				continue
			}
			source, err := resolvePath(destination)
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(source)
			if err != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("missing runtime library for build utility: %s", destination)
			}
			libraries[destination] = source
		}
	}
	result := make([]runtimeLibrary, 0, len(libraries))
	for destination, source := range libraries {
		result = append(result, runtimeLibrary{destination: destination, source: source})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].destination < result[j].destination })
	return result, nil
}

func compileGuestSources(ctx context.Context, files []guest.SnapshotFile, workspace, objects, compiler string, sandbox sandboxMounts, diagnostics *diagnosticLog, timeout time.Duration) ([]string, error) {
	paths := make([]string, 0)
	for _, file := range files {
		if strings.HasSuffix(file.Path, ".c") || strings.HasSuffix(file.Path, ".S") {
			paths = append(paths, file.Path)
		}
	}
	sort.Strings(paths)
	objectPaths := make([]string, 0, len(paths))
	for index, sourcePath := range paths {
		objectPath := filepath.Join(objects, fmt.Sprintf("guest-%d.o", index))
		flags := cFlags
		if strings.HasSuffix(sourcePath, ".S") {
			flags = assemblyFlags
		}
		command := append([]string{compiler}, flags...)
		command = append(command, "-I", "/workspace/seed", "-c", sandboxPath(filepath.Join(workspace, filepath.FromSlash(sourcePath)), workspace), "-o", sandboxPath(objectPath, workspace))
		if err := runSandboxed(ctx, command, workspace, sandbox, diagnostics, timeout); err != nil {
			return nil, err
		}
		objectPaths = append(objectPaths, objectPath)
	}
	return objectPaths, nil
}

func renderEmbeddedSources(files []guest.SnapshotFile) string {
	entries := append([]guest.SnapshotFile(nil), files...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	lines := []string{"#include \"files.h\"", ""}
	for index, entry := range entries {
		lines = append(lines,
			byteArray(fmt.Sprintf("embedded_path_%d", index), []byte(entry.Path), true),
			byteArray(fmt.Sprintf("embedded_content_%d", index), entry.Content, false),
		)
	}
	lines = append(lines, "", "const struct embedded_file initial_files[] = {")
	for index, entry := range entries {
		lines = append(lines,
			"    {",
			fmt.Sprintf("        embedded_path_%d,", index),
			fmt.Sprintf("        %du,", len([]byte(entry.Path))),
			fmt.Sprintf("        embedded_content_%d,", index),
			fmt.Sprintf("        embedded_content_%d + %du,", index, len(entry.Content)),
			"    },",
		)
	}
	lines = append(lines, "};", "", "const uint32_t initial_file_count = sizeof(initial_files) / sizeof(initial_files[0]);")
	return strings.Join(lines, "\n") + "\n"
}

func byteArray(name string, data []byte, readOnly bool) string {
	declaration := "static uint8_t"
	if readOnly {
		declaration = "static const uint8_t"
	}
	if len(data) == 0 {
		return fmt.Sprintf("%s %s[1] = {0};", declaration, name)
	}
	values := make([]string, len(data))
	for index, value := range data {
		values[index] = fmt.Sprintf("0x%02x", value)
	}
	return fmt.Sprintf("%s %s[] = {%s};", declaration, name, strings.Join(values, ", "))
}

func createISO(ctx context.Context, workspace, isoRoot, kernel, limineTool string, limine map[string]string, sandbox sandboxMounts, diagnostics *diagnosticLog, timeout time.Duration) (string, error) {
	boot := filepath.Join(isoRoot, "boot")
	limineDirectory := filepath.Join(boot, "limine")
	if err := os.MkdirAll(limineDirectory, 0o755); err != nil {
		return "", err
	}
	if err := copyFile(kernel, filepath.Join(boot, "kernel.elf")); err != nil {
		return "", err
	}
	for _, name := range []string{"limine.conf", "limine-bios.sys", "limine-bios-cd.bin"} {
		source := filepath.Join(workspace, "seed", "limine.conf")
		if name != "limine.conf" {
			source = limine[name]
		}
		if err := copyFile(source, filepath.Join(limineDirectory, name)); err != nil {
			return "", err
		}
	}

	iso := filepath.Join(workspace, "build", "codexos.iso")
	xorrisoCommand := []string{
		sandbox.xorriso,
		"-as", "mkisofs", "-R", "-r", "-J", "-V", "CODEXOS_SEED",
		"--modification-date=2020010100000000", "--set_all_file_dates", "2020010100000000",
		"-b", "boot/limine/limine-bios-cd.bin", "-no-emul-boot", "-boot-load-size", "4", "-boot-info-table",
		sandboxPath(isoRoot, workspace), "-o", sandboxPath(iso, workspace),
	}
	if err := runSandboxed(ctx, xorrisoCommand, workspace, sandbox, diagnostics, timeout); err != nil {
		return "", err
	}
	if err := runSandboxed(ctx, []string{sandboxPath(limineTool, workspace), "bios-install", sandboxPath(iso, workspace)}, workspace, sandbox, diagnostics, timeout); err != nil {
		return "", err
	}
	return iso, nil
}

func runSandboxed(ctx context.Context, command []string, workspace string, sandbox sandboxMounts, diagnostics *diagnosticLog, timeout time.Duration) error {
	directories := sandboxDirectories(append([]string{sandbox.toolchain, sandbox.xorriso}, libraryDestinations(sandbox.libraries)...))
	wrapped := []string{
		sandbox.bwrap,
		"--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
		"--setenv", "HOME", "/nonexistent", "--setenv", "LC_ALL", "C",
		"--setenv", "PATH", filepath.Join(sandbox.toolchain, "bin") + ":/usr/bin",
		"--setenv", "TMPDIR", "/tmp", "--hostname", "codexos-build", "--cap-drop", "ALL",
		"--dir", "/workspace", "--bind", workspace, "/workspace",
		"--ro-bind", sandbox.trusted, "/workspace/trusted", "--dir", "/tmp", "--tmpfs", "/tmp", "--dev", "/dev",
	}
	for _, directory := range directories {
		wrapped = append(wrapped, "--dir", directory)
	}
	wrapped = append(wrapped,
		"--ro-bind", sandbox.toolchain, sandbox.toolchain,
		"--ro-bind", sandbox.xorriso, sandbox.xorriso,
	)
	for _, library := range sandbox.libraries {
		wrapped = append(wrapped, "--ro-bind", library.source, library.destination)
	}
	wrapped = append(wrapped, "--chdir", "/workspace", "--")
	wrapped = append(wrapped, command...)
	return runStep(ctx, wrapped, workspace, diagnostics, timeout, "")
}

func libraryDestinations(libraries []runtimeLibrary) []string {
	result := make([]string, len(libraries))
	for index, library := range libraries {
		result[index] = library.destination
	}
	return result
}

func sandboxDirectories(paths []string) []string {
	set := make(map[string]struct{})
	for _, path := range paths {
		parent := filepath.Dir(path)
		for parent != string(filepath.Separator) && parent != "." && parent != "" {
			set[parent] = struct{}{}
			parent = filepath.Dir(parent)
		}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Slice(result, func(i, j int) bool {
		depthI := strings.Count(filepath.Clean(result[i]), string(filepath.Separator))
		depthJ := strings.Count(filepath.Clean(result[j]), string(filepath.Separator))
		if depthI != depthJ {
			return depthI < depthJ
		}
		return result[i] < result[j]
	})
	return result
}

func sandboxPath(path, workspace string) string {
	resolved, err := resolvePath(path)
	if err != nil {
		return filepath.ToSlash(filepath.Join("/workspace", "invalid"))
	}
	relative, err := filepath.Rel(workspace, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return filepath.ToSlash(filepath.Join("/workspace", "invalid"))
	}
	return filepath.ToSlash(filepath.Join("/workspace", relative))
}

func captureTrusted(ctx context.Context, command []string, timeout time.Duration) (string, error) {
	var output bytes.Buffer
	buffer := &boundedBuffer{buffer: &output, limit: defaultDiagnosticLimit}
	if err := runStep(ctx, command, "", buffer, timeout, trustedPathEnvironment); err != nil {
		return "", err
	}
	return output.String(), nil
}

func runStep(ctx context.Context, command []string, workspace string, output io.Writer, timeout time.Duration, pathEnvironment string) error {
	if len(command) == 0 {
		return &buildStepError{err: errors.New("empty build command")}
	}
	if ctx == nil {
		return &buildStepError{program: command[0], canceled: true, err: errors.New("nil context")}
	}
	stepContext := ctx
	cancel := func() {}
	if timeout > 0 {
		stepContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	commandProcess := exec.Command(command[0], command[1:]...)
	commandProcess.Dir = workspace
	commandProcess.Stdin = nil
	commandProcess.Stdout = output
	commandProcess.Stderr = output
	if pathEnvironment != "" {
		commandProcess.Env = []string{"LC_ALL=C", "PATH=" + pathEnvironment}
	}
	commandProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := commandProcess.Start(); err != nil {
		return &buildStepError{program: command[0], err: err}
	}
	done := make(chan error, 1)
	go func() { done <- commandProcess.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		return &buildStepError{program: command[0], err: err, exit: exitCode(commandProcess)}
	case <-stepContext.Done():
		killProcessTree(commandProcess)
		reapTimer := time.NewTimer(processReapTimeout)
		select {
		case <-done:
			reapTimer.Stop()
		case <-reapTimer.C:
			return &buildStepError{program: command[0], err: errors.New("build process did not exit after cancellation"), canceled: true}
		}
		if ctx.Err() != nil {
			return &buildStepError{program: command[0], canceled: true, err: ctx.Err()}
		}
		return &buildStepError{program: command[0], timedOut: true, err: context.DeadlineExceeded}
	}
}

func exitCode(command *exec.Cmd) int {
	if command.ProcessState == nil {
		return -1
	}
	return command.ProcessState.ExitCode()
}

func killProcessTree(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		_ = command.Process.Kill()
	}
}

type diagnosticLog struct {
	mutex sync.Mutex
	file  *os.File
	limit int
	used  int
}

func (d *diagnosticLog) Write(data []byte) (int, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.file == nil || d.limit <= d.used {
		return len(data), nil
	}
	remaining := d.limit - d.used
	writeData := data
	if len(writeData) > remaining {
		writeData = writeData[:remaining]
	}
	written, err := d.file.Write(writeData)
	d.used += written
	if err != nil {
		return len(data), err
	}
	// Report the complete input as consumed.  Diagnostics are intentionally
	// bounded without causing a compiler to fail merely because it was noisy.
	return len(data), nil
}

func (d *diagnosticLog) text(message string) string {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.file == nil {
		return truncateDiagnostic(message, d.limit)
	}
	_ = d.file.Sync()
	if _, err := d.file.Seek(0, io.SeekStart); err != nil {
		return truncateDiagnostic(message, d.limit)
	}
	data, err := io.ReadAll(io.LimitReader(d.file, int64(d.limit)))
	if err != nil {
		return truncateDiagnostic(message, d.limit)
	}
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	if message != "" {
		text = message + "\n" + text
	}
	return truncateDiagnostic(text, d.limit)
}

func (d *diagnosticLog) Close() error {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.file == nil {
		return nil
	}
	return d.file.Close()
}

type boundedBuffer struct {
	mutex  sync.Mutex
	buffer *bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		writeData := data
		if len(writeData) > remaining {
			writeData = writeData[:remaining]
		}
		_, _ = b.buffer.Write(writeData)
	}
	return len(data), nil
}

func truncateDiagnostic(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	data := []byte(text)
	if len(data) <= limit {
		return text
	}
	return strings.ToValidUTF8(string(data[:limit]), "\uFFFD")
}

func harnessFailure(err error) BuildResult {
	if err == nil {
		return BuildResult{Status: BuildStatusHarnessFailure}
	}
	return BuildResult{Status: BuildStatusHarnessFailure, Diagnostics: err.Error()}
}

func harnessFailureWithDiagnostics(diagnostics *diagnosticLog, err error) BuildResult {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return BuildResult{Status: BuildStatusHarnessFailure, Diagnostics: diagnostics.text(message)}
}

func buildFailureOrHarness(diagnostics *diagnosticLog, err error) BuildResult {
	message := ""
	if err != nil {
		message = err.Error()
	}
	status := BuildStatusBuildFailure
	var step *buildStepError
	if !errors.As(err, &step) || step.canceled || (step.err != nil && step.exit == 0 && !step.timedOut) {
		status = BuildStatusHarnessFailure
	}
	return BuildResult{Status: status, Diagnostics: diagnostics.text(message)}
}

func findConfiguredExecutable(configured, name string) (string, error) {
	candidate := configured
	if candidate == "" {
		var err error
		candidate, err = exec.LookPath(name)
		if err != nil {
			return "", err
		}
	} else if !strings.ContainsRune(candidate, filepath.Separator) {
		var err error
		candidate, err = exec.LookPath(candidate)
		if err != nil {
			return "", err
		}
	}
	resolved, err := resolvePath(candidate)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("configured build utility is not executable")
	}
	return resolved, nil
}

func discoverRepositoryRoot() (string, error) {
	candidates := make([]string, 0, 2)
	if working, err := os.Getwd(); err == nil {
		candidates = append(candidates, working)
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "../.."))
	}
	for _, candidate := range candidates {
		root := candidate
		for {
			resolved, err := resolvePath(root)
			if err == nil {
				if _, err := os.Stat(filepath.Join(resolved, "third_party", "limine")); err == nil {
					return resolved, nil
				}
			}
			parent := filepath.Dir(root)
			if parent == root {
				break
			}
			root = parent
		}
	}
	return "", errors.New("could not locate CodexOS repository")
}

func resolvePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	missing := make([]string, 0)
	probe := absolute
	for {
		resolved, evalErr := filepath.EvalSymlinks(probe)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return absolute, nil
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func sameOrWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	return nil
}
