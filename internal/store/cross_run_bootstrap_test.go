package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codexos/internal/guest"
	"codexos/internal/qemu"
)

func TestCrossRunBootstrapPreservesSourceHandoffAndFeatureStates(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "experiment-source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, filepath.Base(source)+"/generation-0000")
	initialISO := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	destination := filepath.Join(root, "experiment-destination")

	if _, err := InitializeCrossRunBootstrap(
		destination,
		initialISO,
		source,
		0,
		repository,
		filepath.Base(source)+"/generation-0000",
	); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCrossRunBootstrap(destination)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.SourceRun != filepath.Base(source) || loaded.SourceGeneration != 0 || loaded.Handoff != "Source handoff λ.\n" {
		t.Fatalf("loaded bootstrap = %#v", loaded)
	}
	if !equalCrossRunRequestIDsFromValues(loaded.InheritedRequestIDs, []uint64{1, 2}) {
		t.Fatalf("inherited request IDs = %v", loaded.InheritedRequestIDs)
	}
	store, err := NewFeatureRequestStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := store.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Status != FeaturePending || requests[1].Status != FeatureApproved {
		t.Fatalf("inherited requests = %#v", requests)
	}
}

func TestCrossRunBootstrapAllowsChainedInheritedRequestFromHigherGeneration(t *testing.T) {
	root := t.TempDir()
	predecessor := filepath.Join(root, "experiment-002")
	if err := createCrossRunHistory(predecessor, 10, 10); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "experiment-002/generation-0010")

	middle := filepath.Join(root, "experiment-003")
	predecessorISO := filepath.Join(predecessor, "generation-0010", "successor", "codexos.iso")
	if _, err := InitializeCrossRunBootstrap(
		middle,
		predecessorISO,
		predecessor,
		10,
		repository,
		"experiment-002/generation-0010",
	); err != nil {
		t.Fatal(err)
	}
	if err := createCrossRunHistory(middle, 0, -1); err != nil {
		t.Fatal(err)
	}
	runCrossRunGit(t, repository, "tag", "--annotate", "--no-sign", "--cleanup=verbatim", "-m", "Cross-run base", "experiment-003/generation-0000")

	destination := filepath.Join(root, "experiment-004")
	middleISO := filepath.Join(middle, "generation-0000", "successor", "codexos.iso")
	if _, err := InitializeCrossRunBootstrap(
		destination,
		middleISO,
		middle,
		0,
		repository,
		"experiment-003/generation-0000",
	); err != nil {
		t.Fatal(err)
	}
	requests, err := NewFeatureRequestStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := requests.Requests()
	if err != nil {
		t.Fatal(err)
	}
	if len(inherited) != 2 || inherited[0].Generation != 10 || inherited[1].Generation != 10 {
		t.Fatalf("chained inherited requests = %#v", inherited)
	}

	middleRequests, err := NewFeatureRequestStore(middle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := middleRequests.Create(1, "Unarchived local request", "Must still be rejected."); err != nil {
		t.Fatal(err)
	}
	invalidDestination := filepath.Join(root, "invalid-destination")
	if _, err := InitializeCrossRunBootstrap(
		invalidDestination,
		middleISO,
		middle,
		0,
		repository,
		"experiment-003/generation-0000",
	); err == nil || !strings.Contains(err.Error(), "newer than the inherited generation") {
		t.Fatalf("unarchived local request error = %v", err)
	}
	if _, err := os.Lstat(invalidDestination); !os.IsNotExist(err) {
		t.Fatalf("failed initialization published destination: %v", err)
	}
}

func TestCrossRunBootstrapRejectsIdentityAndLedgerTampering(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	image := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	base := filepath.Join(root, "base")
	if _, err := InitializeCrossRunBootstrap(base, image, source, 0, repository, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, CrossRunBootstrapHandoff), []byte("tampered\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(base); err == nil || !strings.Contains(err.Error(), "handoff identity") {
		t.Fatalf("tampered handoff error = %v", err)
	}

	ledger := filepath.Join(root, "ledger")
	if _, err := InitializeCrossRunBootstrap(ledger, image, source, 0, repository, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(ledger, CrossRunBootstrapFeatureLedger)
	contents, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, append(contents, ' '), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(ledger); err == nil || !strings.Contains(err.Error(), "feature ledger") {
		t.Fatalf("tampered ledger error = %v", err)
	}

	wrongImage := filepath.Join(root, "wrong.iso")
	if err := os.WriteFile(wrongImage, []byte("different"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeCrossRunBootstrap(filepath.Join(root, "mismatch"), wrongImage, source, 0, repository, "source/generation-0000"); err == nil || !strings.Contains(err.Error(), "byte-identical") {
		t.Fatalf("ISO mismatch error = %v", err)
	}
}

func TestCrossRunBootstrapRejectsCollisionsAndInvalidSourceSelectionWithoutPublishing(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	image := filepath.Join(source, "generation-0000", "successor", "codexos.iso")

	destination := filepath.Join(root, "existing")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "operator-state")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeCrossRunBootstrap(destination, image, source, 0, repository, "source/generation-0000"); err == nil || !strings.Contains(err.Error(), "fresh destination") {
		t.Fatalf("destination collision error = %v", err)
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "preserve" {
		t.Fatalf("destination marker = %q, %v", contents, err)
	}

	nonexistent := filepath.Join(root, "nonexistent-generation")
	if _, err := InitializeCrossRunBootstrap(nonexistent, image, source, 1, repository, "source/generation-0001"); err == nil || !strings.Contains(err.Error(), "latest source-run archive") {
		t.Fatalf("source generation error = %v", err)
	}
	if _, err := os.Lstat(nonexistent); !os.IsNotExist(err) {
		t.Fatalf("failed initialization published destination: %v", err)
	}
}

func TestCrossRunBootstrapRequiresAnnotatedGenerationTagAndCompleteTriple(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	runCrossRunGit(t, repository, "tag", "--delete", "source/generation-0000")
	runCrossRunGit(t, repository, "update-ref", "refs/tags/source/generation-0000", "HEAD")
	image := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	destination := filepath.Join(root, "destination")
	if _, err := InitializeCrossRunBootstrap(destination, image, source, 0, repository, "source/generation-0000"); err == nil || !strings.Contains(err.Error(), "not annotated") {
		t.Fatalf("lightweight-tag error = %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("Git validation failure published destination: %v", err)
	}

	partial := filepath.Join(root, "partial")
	if err := os.Mkdir(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, CrossRunBootstrapManifest), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCrossRunBootstrap(partial); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("partial provenance error = %v", err)
	}
}

func createCrossRunFixture(source string) error {
	return createCrossRunHistory(source, 0, 0)
}

func createCrossRunHistory(source string, latestGeneration, requestGeneration int) error {
	snapshot, err := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("source\n")}})
	if err != nil {
		return err
	}
	hardware, err := qemu.TestHardwareProfile.Manifest("QEMU emulator version test")
	if err != nil {
		return err
	}
	hardwareBytes, err := qemu.EncodeHardwareManifest(hardware)
	if err != nil {
		return err
	}
	for generation := 0; generation <= latestGeneration; generation++ {
		archive := filepath.Join(source, fmt.Sprintf("generation-%04d", generation))
		var parent *int
		transition := "initial"
		if generation > 0 {
			previous := generation - 1
			parent = &previous
			transition = "successor"
		}
		files := map[string][]byte{
			"metadata.json": mustJSONBytes(map[string]any{"generation": generation, "outcome": "completed", "parent_generation": parent, "transition": transition}),
			"hardware.json": hardwareBytes, "source.snapshot": snapshot,
			"source/seed/kernel.c": []byte("source\n"), "handoff.txt": []byte("Source handoff λ.\n"),
			"boot/codexos.iso": []byte("boot"), "successor/kernel.elf": []byte("kernel"),
			"successor/codexos.iso": []byte("successor"), "qemu.stdout": {}, "qemu.stderr": {},
		}
		for name, contents := range files {
			path := filepath.Join(archive, name)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(path, contents, 0600); err != nil {
				return err
			}
		}
	}
	if requestGeneration >= 0 {
		requests, err := NewFeatureRequestStore(source)
		if err != nil {
			return err
		}
		if _, err = requests.Create(uint64(requestGeneration), "Pending λ", "Pending source request"); err != nil {
			return err
		}
		approved, err := requests.Create(uint64(requestGeneration), "Approved", "Approved source request")
		if err != nil {
			return err
		}
		if _, err = requests.Approve(approved.ID, ""); err != nil {
			return err
		}
	}
	return nil
}

func createCrossRunGitRepository(t *testing.T, repository, tag string) {
	t.Helper()
	if err := os.MkdirAll(repository, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Cross-run test"},
		{"config", "user.email", "cross-run@example.invalid"},
	} {
		runCrossRunGit(t, repository, arguments...)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("trusted base\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	runCrossRunGit(t, repository, "add", "README.md")
	runCrossRunGit(t, repository, "commit", "-q", "-m", "Trusted experiment base")
	runCrossRunGit(t, repository, "tag", "--annotate", "--no-sign", "--cleanup=verbatim", "-m", "Cross-run base", tag)
}

func runCrossRunGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	commandArguments := append([]string{"-C", repository}, arguments...)
	command := exec.Command("git", commandArguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
}

func equalCrossRunRequestIDsFromValues(left, right []uint64) bool {
	return bytes.Equal(mustJSONBytes(left), mustJSONBytes(right))
}

func mustJSONBytes(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
