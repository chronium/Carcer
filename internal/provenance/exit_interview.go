package provenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// ExitInterviewActivityKind names the only renderable activity retained in an
// interview artifact. Raw reasoning is never accepted by this recorder.
type ExitInterviewActivityKind string

const (
	ExitInterviewReasoningDelta   ExitInterviewActivityKind = "agent.reasoning_delta"
	ExitInterviewReasoningSummary ExitInterviewActivityKind = "agent.reasoning_summary"
)

// ExitInterviewActivity is the trusted, already-filtered representation of one
// app-server item. Data is intentionally unstructured because the
// app-server notification boundary owns its schema.
type ExitInterviewActivity struct {
	Kind   ExitInterviewActivityKind
	Data   map[string]any
	ItemID string
}

type ExitInterviewTranscriptError struct {
	Reason string
	Err    error
}

func (e *ExitInterviewTranscriptError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *ExitInterviewTranscriptError) Unwrap() error { return e.Err }

type ExitInterviewMetadata struct {
	Run                  string
	Generation           uint64
	AgentContractVersion uint64
	Model                string
	ReasoningEffort      string
	ReasoningSummary     string
	ServiceTier          string
}

type ExitInterviewTurn struct {
	Number             int
	Question           string
	ReasoningSummaries []string
	Response           *string
	Status             string
}

type ExitInterviewTranscriptSnapshot struct {
	Metadata ExitInterviewMetadata
	Turns    []ExitInterviewTurn
}

type ExitInterviewArtifact struct {
	Path            string
	RelativePath    string
	AlreadyRecorded bool
}

type reasoningItem struct {
	parts map[int]string
}

func (item *reasoningItem) text() []string {
	indices := make([]int, 0, len(item.parts))
	for index := range item.parts {
		indices = append(indices, index)
	}
	// The number of summary parts is small, and sorting the indices directly
	// preserves Python's numeric-index ordering without imposing an event
	// arrival order on the rendered artifact.
	for index := 1; index < len(indices); index++ {
		value := indices[index]
		position := index
		for position > 0 && indices[position-1] > value {
			indices[position] = indices[position-1]
			position--
		}
		indices[position] = value
	}
	result := make([]string, 0, len(indices))
	for _, index := range indices {
		value := item.parts[index]
		if trimPythonWhitespace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

type interviewTurn struct {
	number         int
	question       string
	turnID         string
	reasoningOrder []string
	reasoning      map[string]*reasoningItem
	response       *string
	status         string
}

func (turn *interviewTurn) reasoningItem(itemID string) *reasoningItem {
	if itemID == "" {
		itemID = "reasoning"
	}
	item := turn.reasoning[itemID]
	if item == nil {
		item = &reasoningItem{parts: make(map[int]string)}
		turn.reasoning[itemID] = item
		turn.reasoningOrder = append(turn.reasoningOrder, itemID)
	}
	return item
}

func (turn *interviewTurn) snapshot() ExitInterviewTurn {
	summaries := make([]string, 0)
	for _, itemID := range turn.reasoningOrder {
		summaries = append(summaries, turn.reasoning[itemID].text()...)
	}
	var response *string
	if turn.response != nil {
		value := *turn.response
		response = &value
	}
	return ExitInterviewTurn{
		Number:             turn.number,
		Question:           turn.question,
		ReasoningSummaries: summaries,
		Response:           response,
		Status:             turn.status,
	}
}

// ExitInterviewTranscript captures one generation-local interview.  It is
// safe for an activity observer and the turn worker to call concurrently.
type ExitInterviewTranscript struct {
	metadata ExitInterviewMetadata
	turns    []*interviewTurn
	active   *interviewTurn
	mu       sync.Mutex
}

func NewExitInterviewTranscript(metadata ExitInterviewMetadata) *ExitInterviewTranscript {
	return &ExitInterviewTranscript{metadata: metadata}
}

func (t *ExitInterviewTranscript) BeginTurn(number int, question, turnID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active != nil {
		return &ExitInterviewTranscriptError{Reason: "an exit-interview transcript turn is active"}
	}
	turn := &interviewTurn{
		number:    number,
		question:  question,
		turnID:    turnID,
		reasoning: make(map[string]*reasoningItem),
		status:    "running",
	}
	t.turns = append(t.turns, turn)
	t.active = turn
	return nil
}

// Observe retains only reasoning deltas and completed reasoning summaries for
// the active matching turn.  Malformed or unrelated activity is deliberately
// ignored, just as it is at the Python visibility boundary.
func (t *ExitInterviewTranscript) Observe(activity ExitInterviewActivity, turnID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.active
	if turn == nil || turn.turnID != turnID {
		return nil
	}
	switch activity.Kind {
	case ExitInterviewReasoningDelta:
		text, textOK := activity.Data["text"].(string)
		index, indexOK := reasoningIndex(activity.Data["summary_index"])
		if !textOK || !indexOK {
			return nil
		}
		item := turn.reasoningItem(activity.ItemID)
		item.parts[index] += text
	case ExitInterviewReasoningSummary:
		summary, ok := reasoningSummary(activity.Data["summary"])
		if !ok {
			return nil
		}
		item := turn.reasoningItem(activity.ItemID)
		item.parts = make(map[int]string, len(summary))
		for index, text := range summary {
			item.parts[index] = text
		}
	}
	return nil
}

func reasoningIndex(value any) (int, bool) {
	switch value := value.(type) {
	case bool:
		// bool is an int subclass in Python, so isinstance(True, int) is
		// accepted by the reference recorder.
		if value {
			return 1, true
		}
		return 0, true
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
	case int64:
		if int64(int(value)) != value {
			return 0, false
		}
		return int(value), true
	case uint:
		if uint(int(value)) != value {
			return 0, false
		}
		return int(value), true
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		if uint32(int(value)) != value {
			return 0, false
		}
		return int(value), true
	case uint64:
		if uint64(int(value)) != value {
			return 0, false
		}
		return int(value), true
	case json.Number:
		parsed, err := value.Int64()
		if err != nil || int64(int(parsed)) != parsed {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func reasoningSummary(value any) ([]string, bool) {
	switch value := value.(type) {
	case []string:
		return append([]string(nil), value...), true
	case []any:
		result := make([]string, len(value))
		for index, part := range value {
			text, ok := part.(string)
			if !ok {
				return nil, false
			}
			result[index] = text
		}
		return result, true
	default:
		return nil, false
	}
}

func (t *ExitInterviewTranscript) FinishTurn(turnID string, response *string, status string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.active
	if turn == nil || turn.turnID != turnID {
		return nil
	}
	if response != nil {
		value := *response
		turn.response = &value
	} else {
		turn.response = nil
	}
	turn.status = status
	t.active = nil
	return nil
}

func (t *ExitInterviewTranscript) Snapshot() ExitInterviewTranscriptSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	turns := make([]ExitInterviewTurn, 0, len(t.turns))
	for _, turn := range t.turns {
		turns = append(turns, turn.snapshot())
	}
	return ExitInterviewTranscriptSnapshot{
		Metadata: t.metadata,
		Turns:    turns,
	}
}

// ExitInterviewArtifactStore adds one immutable, run-scoped artifact to a
// repository worktree.  A hard-link publication makes concurrent finalizers
// choose one byte sequence without ever replacing an existing record.
type ExitInterviewArtifactStore struct {
	repository string
	run        string
}

func NewExitInterviewArtifactStore(repository, runDirectory string) (*ExitInterviewArtifactStore, error) {
	repositoryPath, err := resolvePath(repository)
	if err != nil {
		return nil, &ExitInterviewTranscriptError{Reason: "interview repository is unavailable", Err: err}
	}
	info, err := os.Stat(repositoryPath)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("path is not a directory")
		}
		return nil, &ExitInterviewTranscriptError{Reason: fmt.Sprintf("interview repository is unavailable: %s", repositoryPath), Err: err}
	}
	runPath, err := resolvePath(runDirectory)
	if err != nil {
		return nil, &ExitInterviewTranscriptError{Reason: "run directory cannot form an interview artifact namespace", Err: err}
	}
	run := filepath.Base(runPath)
	if !safeNamespace(run) {
		return nil, &ExitInterviewTranscriptError{Reason: "run directory cannot form an interview artifact namespace"}
	}
	return &ExitInterviewArtifactStore{repository: repositoryPath, run: run}, nil
}

func resolvePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	// pathlib.Path.resolve() is non-strict: an as-yet nonexistent run path
	// still contributes its safe basename to the namespace.
	if errors.Is(err, os.ErrNotExist) {
		return absolute, nil
	}
	return "", err
}

func safeNamespace(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\\`) && !strings.ContainsRune(value, 0) &&
		utf8.ValidString(value)
}

func (s *ExitInterviewArtifactStore) Persist(transcript ExitInterviewTranscriptSnapshot, outcome string) (*ExitInterviewArtifact, error) {
	if len(transcript.Turns) == 0 {
		return nil, nil
	}
	if transcript.Metadata.Run != s.run {
		return nil, &ExitInterviewTranscriptError{Reason: "interview transcript belongs to another run"}
	}
	if err := validateTranscriptUTF8(transcript, outcome); err != nil {
		return nil, err
	}
	relative := filepath.Join("artifacts", "interviews", s.run, fmt.Sprintf("generation-%04d.md", transcript.Metadata.Generation))
	output := filepath.Join(s.repository, relative)
	contents := []byte(RenderExitInterviewMarkdown(transcript, outcome))

	// Validate every directory component before inspecting the output.  This
	// prevents a pre-existing symlinked parent from becoming an alternate
	// artifact namespace.
	directory, err := s.ensureDirectory(filepath.Dir(relative))
	if err != nil {
		return nil, err
	}
	existing, present, err := existingArtifact(output)
	if err != nil {
		return nil, err
	}
	if present {
		if bytes.Equal(existing, contents) {
			return &ExitInterviewArtifact{Path: output, RelativePath: relative, AlreadyRecorded: true}, nil
		}
		return nil, conflictingArtifact(relative, false)
	}

	temporary, err := os.CreateTemp(directory, ".generation-interview-*.tmp")
	if err != nil {
		return nil, artifactWriteError(output, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	closeTemporary := func() {
		_ = temporary.Close()
	}
	if err := temporary.Chmod(0o644); err != nil {
		closeTemporary()
		return nil, artifactWriteError(output, err)
	}
	if err := writeAll(temporary, contents); err != nil {
		closeTemporary()
		return nil, artifactWriteError(output, err)
	}
	if err := temporary.Sync(); err != nil {
		closeTemporary()
		return nil, artifactWriteError(output, err)
	}
	if err := temporary.Close(); err != nil {
		return nil, artifactWriteError(output, err)
	}

	if err := os.Link(temporaryName, output); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, artifactWriteError(output, err)
		}
		existing, present, inspectErr := existingArtifact(output)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if present && bytes.Equal(existing, contents) {
			return &ExitInterviewArtifact{Path: output, RelativePath: relative, AlreadyRecorded: true}, nil
		}
		return nil, conflictingArtifact(relative, true)
	}
	if err := syncDirectory(directory); err != nil {
		// The published inode remains immutable.  A caller can retry and obtain
		// the idempotent result after a transient directory-sync failure.
		return nil, artifactWriteError(output, err)
	}
	return &ExitInterviewArtifact{Path: output, RelativePath: relative}, nil
}

// Finalize is the lifecycle-oriented spelling of Persist.  Both operations
// perform the same immutable publication.
func (s *ExitInterviewArtifactStore) Finalize(transcript ExitInterviewTranscriptSnapshot, outcome string) (*ExitInterviewArtifact, error) {
	return s.Persist(transcript, outcome)
}

func (s *ExitInterviewArtifactStore) ensureDirectory(relative string) (string, error) {
	current := s.repository
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		candidate := filepath.Join(current, part)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(candidate, 0o777); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return "", directoryError(candidate, err)
				}
			}
			info, err = os.Lstat(candidate)
		}
		if err != nil {
			return "", directoryError(candidate, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", &ExitInterviewTranscriptError{Reason: fmt.Sprintf("interview artifact directory must not be a symlink: %s", candidate)}
		}
		if !info.IsDir() {
			return "", &ExitInterviewTranscriptError{Reason: fmt.Sprintf("interview artifact directory is unavailable: %s", candidate)}
		}
		current = candidate
	}
	return current, nil
}

func existingArtifact(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &ExitInterviewTranscriptError{Reason: fmt.Sprintf("could not inspect exit-interview artifact: %s", path), Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, &ExitInterviewTranscriptError{Reason: fmt.Sprintf("exit-interview artifact must not be a symlink: %s", path)}
	}
	if !info.Mode().IsRegular() {
		return nil, false, &ExitInterviewTranscriptError{Reason: fmt.Sprintf("exit-interview artifact path is not a file: %s", path)}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, false, &ExitInterviewTranscriptError{Reason: fmt.Sprintf("could not inspect exit-interview artifact: %s", path), Err: err}
	}
	return contents, true, nil
}

func validateTranscriptUTF8(transcript ExitInterviewTranscriptSnapshot, outcome string) error {
	values := []string{
		transcript.Metadata.Run,
		transcript.Metadata.Model,
		transcript.Metadata.ReasoningEffort,
		transcript.Metadata.ReasoningSummary,
		transcript.Metadata.ServiceTier,
		outcome,
	}
	for _, turn := range transcript.Turns {
		values = append(values, turn.Question, turn.Status)
		values = append(values, turn.ReasoningSummaries...)
		if turn.Response != nil {
			values = append(values, *turn.Response)
		}
	}
	for _, value := range values {
		if !utf8.ValidString(value) {
			return &ExitInterviewTranscriptError{Reason: "exit-interview text must be valid UTF-8"}
		}
	}
	return nil
}

func directoryError(path string, err error) error {
	return &ExitInterviewTranscriptError{Reason: fmt.Sprintf("interview artifact directory is unavailable: %s", path), Err: err}
}

func conflictingArtifact(relative string, appeared bool) error {
	if appeared {
		return &ExitInterviewTranscriptError{Reason: "conflicting exit-interview artifact appeared while recording: " + relative}
	}
	return &ExitInterviewTranscriptError{Reason: "conflicting exit-interview artifact already exists: " + relative}
}

func artifactWriteError(path string, err error) error {
	return &ExitInterviewTranscriptError{Reason: fmt.Sprintf("cannot write exit-interview artifact %s", filepath.Base(path)), Err: err}
}

func writeAll(file *os.File, value []byte) error {
	for len(value) > 0 {
		written, err := file.Write(value)
		if written > 0 {
			value = value[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func RenderExitInterviewMarkdown(transcript ExitInterviewTranscriptSnapshot, outcome string) string {
	metadata := transcript.Metadata
	lines := []string{
		"# CodexOS Exit Interview",
		"",
		"Run: " + NormalizeExitInterviewText(metadata.Run),
		fmt.Sprintf("Generation: %d", metadata.Generation),
		fmt.Sprintf("Agent Contract: %d", metadata.AgentContractVersion),
		"Model: " + NormalizeExitInterviewText(metadata.Model),
		"Reasoning effort: " + NormalizeExitInterviewText(metadata.ReasoningEffort),
		"Reasoning summary: " + NormalizeExitInterviewText(metadata.ReasoningSummary),
		"Service tier: " + NormalizeExitInterviewText(metadata.ServiceTier),
		"Interview status: " + NormalizeExitInterviewText(outcome),
	}
	for _, turn := range transcript.Turns {
		lines = append(lines,
			"",
			fmt.Sprintf("## Question %d", turn.Number),
			"",
			"### Operator",
			"",
			rtrimNewline(NormalizeExitInterviewText(turn.Question)),
		)
		if len(turn.ReasoningSummaries) > 0 {
			summaries := make([]string, len(turn.ReasoningSummaries))
			for index, summary := range turn.ReasoningSummaries {
				summaries[index] = rtrimNewline(NormalizeExitInterviewText(summary))
			}
			lines = append(lines, "", "### Sol — reasoning summary", "", strings.Join(summaries, "\n\n"))
		}
		if turn.Response != nil {
			lines = append(lines, "", "### Sol", "", rtrimNewline(NormalizeExitInterviewText(*turn.Response)))
		}
		if turn.Status != "completed" {
			lines = append(lines, "", "Turn status: "+NormalizeExitInterviewText(turn.Status))
		}
	}
	return strings.TrimRightFunc(strings.Join(lines, "\n"), unicode.IsSpace) + "\n"
}

func rtrimNewline(value string) string {
	return strings.TrimRight(value, "\n")
}

func trimPythonWhitespace(value string) string {
	return strings.TrimFunc(value, func(character rune) bool {
		// Python's str.strip() follows Unicode whitespace and additionally
		// treats the four ASCII information separators as whitespace.
		return unicode.IsSpace(character) || character >= 0x1c && character <= 0x1f
	})
}

// NormalizeExitInterviewText makes untrusted text safe for a human-facing
// Markdown artifact while retaining ordinary Unicode, tabs, and line breaks.
func NormalizeExitInterviewText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var output strings.Builder
	output.Grow(len(text))
	for _, character := range text {
		codepoint := rune(character)
		if character == '\n' || character == '\t' || (codepoint >= 0x20 && !(codepoint >= 0x7f && codepoint <= 0x9f)) {
			output.WriteRune(character)
		} else if codepoint <= 0xff {
			fmt.Fprintf(&output, `\x%02x`, codepoint)
		} else {
			fmt.Fprintf(&output, `\u%04x`, codepoint)
		}
	}
	return output.String()
}

func normalizeExitInterviewText(text string) string { return NormalizeExitInterviewText(text) }
