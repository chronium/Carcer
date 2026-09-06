package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexos/internal/provenance"
)

func TestCrossRunBootstrapCarriesSourceAndDestinationHarnessIdentity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	sourceIdentity := crossRunHarnessTestIdentity("a", "b")
	encoded, err := provenance.EncodeHarnessIdentity(sourceIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "generation-0000", provenance.GenerationHarnessFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	destinationIdentity := crossRunHarnessTestIdentity("c", "d")
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	destination := filepath.Join(root, "destination")
	bootstrap, err := InitializeCrossRunBootstrapWithHarnessIdentity(
		destination, filepath.Join(source, "generation-0000", "successor", "codexos.iso"),
		source, 0, repository, "source/generation-0000", &destinationIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.SourceHarnessIdentity == nil || !bootstrap.SourceHarnessIdentity.Equal(sourceIdentity) ||
		bootstrap.DestinationHarnessIdentity == nil || !bootstrap.DestinationHarnessIdentity.Equal(destinationIdentity) {
		t.Fatalf("bootstrap identities = %#v / %#v", bootstrap.SourceHarnessIdentity, bootstrap.DestinationHarnessIdentity)
	}
	loaded, err := LoadCrossRunBootstrap(destination)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SourceHarnessIdentity == nil || !loaded.SourceHarnessIdentity.Equal(sourceIdentity) ||
		loaded.DestinationHarnessIdentity == nil || !loaded.DestinationHarnessIdentity.Equal(destinationIdentity) {
		t.Fatalf("loaded identities = %#v / %#v", loaded.SourceHarnessIdentity, loaded.DestinationHarnessIdentity)
	}
	manifest, err := os.ReadFile(filepath.Join(destination, CrossRunBootstrapManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"schema_version": 3`) {
		t.Fatalf("identity-aware bootstrap did not use schema 3: %s", manifest)
	}
}

func TestCrossRunBootstrapPreservesUnavailableLegacySourceHarnessIdentity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	destinationIdentity := crossRunHarnessTestIdentity("c", "d")
	destination := filepath.Join(root, "destination")
	if _, err := InitializeCrossRunBootstrapWithHarnessIdentity(
		destination, filepath.Join(source, "generation-0000", "successor", "codexos.iso"),
		source, 0, repository, "source/generation-0000", &destinationIdentity,
	); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCrossRunBootstrap(destination)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SourceHarnessIdentity != nil {
		t.Fatalf("legacy source identity was fabricated: %#v", loaded.SourceHarnessIdentity)
	}
}

func TestCrossRunInspectionRetainsAbortReasonWithoutInheritingFeedback(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunHistory(source, 2, -1); err != nil {
		t.Fatal(err)
	}
	aborted := filepath.Join(source, "generation-0001")
	for _, name := range []string{"handoff.txt", "source.snapshot", "source", "successor"} {
		if err := os.RemoveAll(filepath.Join(aborted, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(aborted, "metadata.json"), []byte("{\n  \"generation\": 1,\n  \"outcome\": \"aborted\",\n  \"parent_generation\": 0,\n  \"transition\": \"successor\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aborted, "aborted.txt"), []byte("Generation aborted by operator."), 0o600); err != nil {
		t.Fatal(err)
	}
	reason := "source operator observed λ\nexact historical reason"
	if err := os.WriteFile(filepath.Join(aborted, "abort-reason.txt"), []byte(reason), 0o600); err != nil {
		t.Fatal(err)
	}
	latest := filepath.Join(source, "generation-0002")
	if err := os.WriteFile(filepath.Join(latest, "metadata.json"), []byte("{\n  \"generation\": 2,\n  \"outcome\": \"completed\",\n  \"parent_generation\": 0,\n  \"transition\": \"rollback\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	feedbackDirectory := filepath.Join(source, "operator-feedback")
	if err := os.Mkdir(feedbackDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	feedback := "{\n  \"target_generation\": 2,\n  \"source_abort_generation\": 1,\n  \"reason\": \"source operator observed λ\\nexact historical reason\",\n  \"schema_version\": 1\n}\n"
	if err := os.WriteFile(filepath.Join(feedbackDirectory, "generation-0002.json"), []byte(feedback), 0o600); err != nil {
		t.Fatal(err)
	}

	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0002")
	destination := filepath.Join(root, "destination")
	if _, err := InitializeCrossRunBootstrap(
		destination, filepath.Join(latest, "successor", "codexos.iso"),
		source, 2, repository, "source/generation-0002",
	); err != nil {
		t.Fatal(err)
	}
	if persisted, err := os.ReadFile(filepath.Join(aborted, "abort-reason.txt")); err != nil || string(persisted) != reason {
		t.Fatalf("cross-run inspection changed historical reason = %q, %v", persisted, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "operator-feedback")); !os.IsNotExist(err) {
		t.Fatalf("cross-run execution inherited operator feedback: %v", err)
	}
	bootstrap, err := LoadCrossRunBootstrap(destination)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bootstrap.Handoff, reason) {
		t.Fatalf("abort reason leaked into inherited handoff: %q", bootstrap.Handoff)
	}
}

func crossRunHarnessTestIdentity(commit, executable string) provenance.HarnessIdentity {
	return provenance.HarnessIdentity{
		SchemaVersion:    provenance.HarnessIdentitySchemaVersion,
		RepositoryCommit: strings.Repeat(commit, 40),
		Executable:       provenance.FileIdentity{SHA256: strings.Repeat(executable, 64), Size: 10},
		Build: provenance.HarnessBuildIdentity{
			GoVersion: "go-test", ModulePath: "codexos", ModuleVersion: "v-test",
			SettingsSHA256: strings.Repeat("e", 64),
		},
	}
}
