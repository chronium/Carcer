package provenance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

func TestPythonExitInterviewTranscriptConformance(t *testing.T) {
	root := provenanceRepositoryRoot(t)
	pythonRepository := t.TempDir()
	pythonRun := filepath.Join(t.TempDir(), "experiment-conformance")
	goRepository := t.TempDir()
	goRun := filepath.Join(t.TempDir(), "experiment-conformance")
	for _, path := range []string{pythonRun, goRun} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const script = `
import importlib.util, pathlib, sys, types
root = pathlib.Path(sys.argv[1])
repository = pathlib.Path(sys.argv[2])
run = pathlib.Path(sys.argv[3])
harness = types.ModuleType("harness")
harness.__path__ = [str(root / "harness")]
sys.modules["harness"] = harness
activity_spec = importlib.util.spec_from_file_location("harness.codex_activity", root / "harness" / "codex_activity.py")
activity_module = importlib.util.module_from_spec(activity_spec)
sys.modules[activity_spec.name] = activity_module
activity_spec.loader.exec_module(activity_module)
transcript_spec = importlib.util.spec_from_file_location("harness.exit_interview_transcript", root / "harness" / "exit_interview_transcript.py")
transcript_module = importlib.util.module_from_spec(transcript_spec)
sys.modules[transcript_spec.name] = transcript_module
transcript_spec.loader.exec_module(transcript_module)
CodexActivityKind = activity_module.CodexActivityKind
RenderableCodexActivity = activity_module.RenderableCodexActivity
ExitInterviewArtifactStore = transcript_module.ExitInterviewArtifactStore
ExitInterviewMetadata = transcript_module.ExitInterviewMetadata
ExitInterviewTranscript = transcript_module.ExitInterviewTranscript
metadata = ExitInterviewMetadata(run.name, 17, 4, "gpt-5.6-sol λ", "high", "auto", "priority")
transcript = ExitInterviewTranscript(metadata)
transcript.begin_turn(1, "Why?\r\nExact\rquestion", "turn-1")
transcript.observe(RenderableCodexActivity(CodexActivityKind.AGENT_REASONING_DELTA, {"text": "Visible ", "summary_index": 0}, "reasoning"), "turn-1")
transcript.observe(RenderableCodexActivity(CodexActivityKind.AGENT_REASONING_DELTA, {"text": "partial Ω\rsummary", "summary_index": 0}, "reasoning"), "turn-1")
ExitInterviewArtifactStore(repository, run).persist(transcript.snapshot(), "interrupted")
`
	command := exec.Command("python3", "-c", script, root, pythonRepository, pythonRun)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python exit-interview reference failed: %v\n%s", err, output)
	}

	transcript := NewExitInterviewTranscript(ExitInterviewMetadata{
		Run:                  filepath.Base(goRun),
		Generation:           17,
		AgentContractVersion: 4,
		Model:                "gpt-5.6-sol λ",
		ReasoningEffort:      "high",
		ReasoningSummary:     "auto",
		ServiceTier:          "priority",
	})
	if err := transcript.BeginTurn(1, "Why?\r\nExact\rquestion", "turn-1"); err != nil {
		t.Fatal(err)
	}
	for _, activity := range []ExitInterviewActivity{
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "Visible ", "summary_index": 0}, ItemID: "reasoning"},
		{Kind: ExitInterviewReasoningDelta, Data: map[string]any{"text": "partial Ω\rsummary", "summary_index": 0}, ItemID: "reasoning"},
	} {
		_ = transcript.Observe(activity, "turn-1")
	}
	store, err := NewExitInterviewArtifactStore(goRepository, goRun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Persist(transcript.Snapshot(), "interrupted"); err != nil {
		t.Fatal(err)
	}

	pythonFiles := conformanceFiles(t, pythonRepository)
	goFiles := conformanceFiles(t, goRepository)
	if len(pythonFiles) != len(goFiles) {
		t.Fatalf("file counts differ: Python %v Go %v", pythonFiles, goFiles)
	}
	for index, relative := range pythonFiles {
		if goFiles[index] != relative {
			t.Fatalf("file lists differ: Python %v Go %v", pythonFiles, goFiles)
		}
		pythonContents, err := os.ReadFile(filepath.Join(pythonRepository, relative))
		if err != nil {
			t.Fatal(err)
		}
		goContents, err := os.ReadFile(filepath.Join(goRepository, relative))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(goContents, pythonContents) {
			t.Fatalf("%s differs:\nGo: %q\nPython: %q", relative, goContents, pythonContents)
		}
	}
}

func conformanceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}
