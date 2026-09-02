//go:build linux

package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"codexos/internal/guest"
	"codexos/internal/qemu"
)

const realImageAcceptanceEnvironment = "CODEXOS_REAL_IMAGE_ACCEPTANCE"

func TestRealSeedImageBuildsAndPassesCandidateValidation(t *testing.T) {
	if os.Getenv(realImageAcceptanceEnvironment) != "1" {
		t.Skipf("set %s=1 to run the real toolchain/QEMU acceptance", realImageAcceptanceEnvironment)
	}

	tools := ToolPaths{
		Bwrap:         realImageTool(t, "bwrap"),
		CC:            realImageTool(t, "cc"),
		LDD:           realImageTool(t, "ldd"),
		CrossCompiler: realImageTool(t, "x86_64-elf-gcc"),
		CrossLinker:   realImageTool(t, "x86_64-elf-ld"),
		Xorriso:       realImageTool(t, "xorriso"),
	}
	qemuExecutable := realImageTool(t, "qemu-system-x86_64")
	repositoryRoot := buildRepositoryRoot(t)
	snapshot := realSeedSnapshot(t, repositoryRoot)
	root, err := os.MkdirTemp("/tmp", "codexos-real-image-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	built := BuildSourceSnapshot(ctx, snapshot, filepath.Join(root, "compiled"), Config{
		RepositoryRoot: repositoryRoot,
		Tools:          tools,
	})
	if built.Status != BuildStatusSuccess {
		t.Fatalf("real seed build = %s: %s", built.Status, built.Diagnostics)
	}
	for _, artifact := range []string{built.KernelELF, built.ISO} {
		info, err := os.Stat(artifact)
		if err != nil {
			t.Fatalf("stat real build artifact %q: %v", artifact, err)
		}
		if info.Size() == 0 {
			t.Fatalf("real build artifact %q is empty", artifact)
		}
	}

	for _, profile := range []qemu.HardwareProfile{
		qemu.TestHardwareProfile,
		qemu.ExperimentHardwareProfile,
	} {
		t.Run(profile.Profile, func(t *testing.T) {
			if err := profile.RequireAvailable(); err != nil {
				t.Skip(err)
			}
			validator, err := NewCandidateBootValidator(CandidateBootConfig{
				QEMUExecutable:  qemuExecutable,
				HardwareProfile: profile,
				ReadyTimeout:    15 * time.Second,
				TemporaryParent: root,
			})
			if err != nil {
				t.Fatal(err)
			}
			validated := validator.Validate(ctx, built.ISO, nil, nil)
			if validated.Status != BuildStatusSuccess {
				t.Fatalf("real seed candidate validation = %s: %s", validated.Status, validated.Diagnostics)
			}
			workspaces, err := filepath.Glob(filepath.Join(root, "codexos-candidate-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(workspaces) != 0 {
				t.Fatalf("real candidate workspaces survived: %v", workspaces)
			}
		})
	}
}

func realImageTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("real-image acceptance requires %s: %v", name, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func realSeedSnapshot(t *testing.T, repositoryRoot string) []byte {
	t.Helper()
	paths := []string{
		"seed/build.c",
		"seed/build.h",
		"seed/files.c",
		"seed/files.h",
		"seed/kernel.c",
		"seed/limine.conf",
		"seed/linker.ld",
		"seed/protocol.c",
		"seed/protocol.h",
		"seed/serial.c",
		"seed/serial.h",
		"seed/source_snapshot.c",
		"seed/source_snapshot.h",
		"seed/tools.c",
		"seed/tools.h",
	}
	files := make([]guest.SnapshotFile, 0, len(paths))
	for _, path := range paths {
		content, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read canonical seed input %q: %v", path, err)
		}
		files = append(files, guest.SnapshotFile{Path: path, Content: content})
	}
	snapshot, err := guest.EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
