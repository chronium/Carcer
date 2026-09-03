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
	if err := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); err != nil {
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
	if err := createPythonCrossRunFixture(source, repositoryRootForCrossRunTest(t)); err != nil {
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
