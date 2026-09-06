// Package tui contains the presentation model used by the operator frontend.
//
// The model deliberately has no terminal or frontend dependency.  It turns
// the trusted activity stream into bounded, typed display entries; a future
// Bubble Tea view can consume those entries without having to rediscover
// semantics by parsing rendered text.
package tui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"codexos/internal/observability"
)

const (
	DefaultDisplayBytes      = 64 * 1024
	DefaultScrollbackBytes   = 2 * 1024 * 1024
	DefaultScrollbackEntries = 800
	SummaryDisplayBytes      = 1024
)

// ActivityDisplayKind identifies one logical row in the operator transcript.
type ActivityDisplayKind string

const (
	ActivityDisplayKindMessage           ActivityDisplayKind = "message"
	ActivityDisplayKindReasoning         ActivityDisplayKind = "reasoning"
	ActivityDisplayKindTool              ActivityDisplayKind = "tool"
	ActivityDisplayKindFeatureRequest    ActivityDisplayKind = "feature-request"
	ActivityDisplayKindBuild             ActivityDisplayKind = "build"
	ActivityDisplayKindOperator          ActivityDisplayKind = "operator"
	ActivityDisplayKindInterviewQuestion ActivityDisplayKind = "interview-question"
	ActivityDisplayKindLifecycle         ActivityDisplayKind = "lifecycle"
	ActivityDisplayKindNotice            ActivityDisplayKind = "notice"
)

// ActivityDisplayState is the state shown for tools and build phases.
type ActivityDisplayState string

const (
	ActivityDisplayStatePending     ActivityDisplayState = "pending"
	ActivityDisplayStateRunning     ActivityDisplayState = "running"
	ActivityDisplayStateCompleted   ActivityDisplayState = "completed"
	ActivityDisplayStateFailed      ActivityDisplayState = "failed"
	ActivityDisplayStateInterrupted ActivityDisplayState = "interrupted"
	ActivityDisplayStateCancelled   ActivityDisplayState = "cancelled"
)

// FeatureRequestRecordingState describes only the trusted recording attempt.
// It intentionally does not represent the later operator decision.
type FeatureRequestRecordingState string

const (
	FeatureRequestRecordingStateRecording FeatureRequestRecordingState = "recording"
	FeatureRequestRecordingStateRecorded  FeatureRequestRecordingState = "recorded"
	FeatureRequestRecordingStateFailed    FeatureRequestRecordingState = "failed"
)

// FeatureRequestInitialStatus is the status assigned when a request is
// created.  It is not a live view of the feature ledger.
type FeatureRequestInitialStatus string

const FeatureRequestInitialStatusPending FeatureRequestInitialStatus = "pending"

// AgentMessagePresentation is a visible agent message.  TurnPhase is empty
// when the activity did not identify a phase.
type AgentMessagePresentation struct {
	Role      observability.ActivityRole
	Text      string
	Finalized bool
	TurnPhase string
}

// ReasoningPresentation is an explicitly exposed reasoning summary.  It is
// never reconstructed from private/raw reasoning notifications.
type ReasoningPresentation struct {
	Role      observability.ActivityRole
	Text      string
	Finalized bool
	TurnPhase string
}

// ToolDetailPresentation is bounded detail associated with a tool row.
// LineCount is nil for binary payloads.
type ToolDetailPresentation struct {
	Text      string
	ByteCount int
	LineCount *int
	Binary    bool
	Truncated bool
}

// ToolPresentation is the structured, compact view of a dynamic tool call.
type ToolPresentation struct {
	Role       observability.ActivityRole
	Tool       string
	TurnPhase  string
	State      ActivityDisplayState
	Summary    string
	Detail     *ToolDetailPresentation
	ResultNote string
}

// FeatureRequestPresentation displays recording state while deliberately
// avoiding any implication that the requested capability was provisioned.
type FeatureRequestPresentation struct {
	Role           observability.ActivityRole
	RecordingState FeatureRequestRecordingState
	InitialStatus  *FeatureRequestInitialStatus
	Title          string
	Description    string
	RequestID      string
	Error          string
}

type BuildPhasePresentation struct {
	Name  string
	State ActivityDisplayState
}

type BuildPresentation struct {
	State      ActivityDisplayState
	Phases     []BuildPhasePresentation
	Diagnostic string
}

// OperatorPresentation groups startup output or one command's output into a
// stable row.  A nil Command represents startup output or an interview
// question's operator block.
type OperatorPresentation struct {
	Command   *string
	Output    string
	Finalized bool
}

type InterviewQuestionPresentation struct {
	Text string
}

type LifecyclePresentation struct {
	Role   observability.ActivityRole
	Title  string
	Detail string
	State  ActivityDisplayState
}

type NoticePresentation struct {
	Title string
	Text  string
}

// ActivityPresentation is one of the typed values above.
type ActivityPresentation interface{}

// ActivityDisplayEntry is one logical, safely renderable item in scrollback.
type ActivityDisplayEntry struct {
	Key          string
	Kind         ActivityDisplayKind
	Presentation ActivityPresentation
}

// SizeBytes reports the UTF-8 size used by scrollback accounting.
func (e ActivityDisplayEntry) SizeBytes() int {
	return len([]byte(presentationText(e.Presentation)))
}

// ActivityFollowState is the small, frontend-independent state machine for
// live-follow and unread activity.
type ActivityFollowState struct {
	Following bool
	NewEvents int
	ScrollY   float64
}

func NewActivityFollowState() ActivityFollowState {
	return ActivityFollowState{Following: true}
}

func (s *ActivityFollowState) Scrolled(scrollY float64) {
	s.Following = false
	s.ScrollY = scrollY
}

func (s *ActivityFollowState) Arrived(count ...int) {
	amount := 1
	if len(count) != 0 {
		amount = count[0]
	}
	if !s.Following {
		s.NewEvents += amount
	}
}

func (s *ActivityFollowState) ReturnToLive() {
	s.Following = true
	s.NewEvents = 0
}

// ActivityModelOptions controls the three independent display bounds. Zero
// values use the corresponding defaults; negative values are rejected.
type ActivityModelOptions struct {
	MaxEntries   int
	MaxBytes     int
	DisplayBytes int
}

// OperatorActivityModel coalesces semantic activity into bounded, terminal-
// safe display items.
type OperatorActivityModel struct {
	maxEntries   int
	maxBytes     int
	displayBytes int

	entries   []ActivityDisplayEntry
	positions map[string]int

	messageText       map[string]string
	reasoningText     map[string]map[int]string
	toolPresentations map[string]ToolPresentation

	buildNumber    int
	activeBuildKey string
	builds         map[string]BuildPresentation

	operatorNumber    int
	activeOperatorKey string

	discarded             bool
	latestReviewerMessage *textIdentity
	revision              uint64
}

// NewOperatorActivityModel constructs a model with the default bounds unless
// options are supplied.  The constructor returns an error for invalid bounds
// rather than allowing a model that cannot maintain its marker invariant.
func NewOperatorActivityModel(options ...ActivityModelOptions) (*OperatorActivityModel, error) {
	if len(options) > 1 {
		return nil, errors.New("at most one TUI activity model options value is allowed")
	}
	configured := ActivityModelOptions{}
	if len(options) == 1 {
		configured = options[0]
	}
	maxEntries := configured.MaxEntries
	if maxEntries == 0 {
		maxEntries = DefaultScrollbackEntries
	}
	maxBytes := configured.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultScrollbackBytes
	}
	displayBytes := configured.DisplayBytes
	if displayBytes == 0 {
		displayBytes = DefaultDisplayBytes
	}
	if maxEntries < 2 || maxBytes < 1 || displayBytes < 1 {
		return nil, errors.New("TUI display bounds must be positive")
	}
	return &OperatorActivityModel{
		maxEntries:        maxEntries,
		maxBytes:          maxBytes,
		displayBytes:      displayBytes,
		positions:         make(map[string]int),
		messageText:       make(map[string]string),
		reasoningText:     make(map[string]map[int]string),
		toolPresentations: make(map[string]ToolPresentation),
		builds:            make(map[string]BuildPresentation),
	}, nil
}

// Entries returns a snapshot of the current transcript order.  Presentation
// values are treated as immutable by the model.
func (m *OperatorActivityModel) Entries() []ActivityDisplayEntry {
	if m == nil {
		return nil
	}
	entries := make([]ActivityDisplayEntry, len(m.entries))
	for index, entry := range m.entries {
		entries[index] = entry
		entries[index].Presentation = clonePresentation(entry.Presentation)
	}
	return entries
}

func clonePresentation(presentation ActivityPresentation) ActivityPresentation {
	switch value := presentation.(type) {
	case ToolPresentation:
		if value.Detail != nil {
			detail := *value.Detail
			if value.Detail.LineCount != nil {
				lineCount := *value.Detail.LineCount
				detail.LineCount = &lineCount
			}
			value.Detail = &detail
		}
		return value
	case FeatureRequestPresentation:
		if value.InitialStatus != nil {
			status := *value.InitialStatus
			value.InitialStatus = &status
		}
		return value
	case BuildPresentation:
		value.Phases = append([]BuildPhasePresentation(nil), value.Phases...)
		return value
	case OperatorPresentation:
		if value.Command != nil {
			command := *value.Command
			value.Command = &command
		}
		return value
	default:
		return presentation
	}
}

// Revision increases whenever an entry is inserted, changed, removed, or
// scrollback trimming first discards older activity.
func (m *OperatorActivityModel) Revision() uint64 {
	if m == nil {
		return 0
	}
	return m.revision
}

// BeginOperatorBlock starts a startup or command output row and returns its
// stable key.  command nil means the block is not associated with a command.
func (m *OperatorActivityModel) BeginOperatorBlock(command ...*string) string {
	m.FinishOperatorBlock()
	m.operatorNumber++
	key := "operator:" + strconv.Itoa(m.operatorNumber)
	m.activeOperatorKey = key
	var renderedCommand *string
	if len(command) > 1 {
		panic("BeginOperatorBlock accepts at most one command")
	}
	if len(command) == 1 && command[0] != nil {
		value := SafeDisplayText(*command[0], SummaryDisplayBytes)
		renderedCommand = &value
	}
	m.upsertEntry(ActivityDisplayEntry{
		Key:          key,
		Kind:         ActivityDisplayKindOperator,
		Presentation: OperatorPresentation{Command: renderedCommand},
	})
	return key
}

// AppendOperatorOutput appends one safely-rendered output chunk to the active
// block, creating a startup block when necessary.
func (m *OperatorActivityModel) AppendOperatorOutput(text string) bool {
	before := m.revision
	key := m.activeOperatorKey
	if key == "" {
		key = m.BeginOperatorBlock(nil)
	}
	position, ok := m.positions[key]
	if !ok {
		// A bounded trim can discard the active block.  Preserve the Python
		// model's recoverable behavior by creating a fresh block.
		key = m.BeginOperatorBlock(nil)
		position = m.positions[key]
	}
	entry := m.entries[position]
	presentation, ok := entry.Presentation.(OperatorPresentation)
	if !ok {
		panic("active operator block has the wrong presentation")
	}
	rendered := SafeDisplayText(text, m.displayBytes)
	separator := ""
	if presentation.Output != "" {
		separator = "\n"
	}
	presentation.Output = SafeDisplayText(presentation.Output+separator+rendered, m.displayBytes)
	entry.Presentation = presentation
	m.upsertEntry(entry)
	return m.revision != before
}

// FinishOperatorBlock marks the active operator block complete.
func (m *OperatorActivityModel) FinishOperatorBlock() bool {
	key := m.activeOperatorKey
	if key == "" {
		return false
	}
	m.activeOperatorKey = ""
	position, ok := m.positions[key]
	if !ok {
		return false
	}
	entry := m.entries[position]
	presentation, ok := entry.Presentation.(OperatorPresentation)
	if !ok {
		return false
	}
	before := m.revision
	presentation.Finalized = true
	entry.Presentation = presentation
	m.upsertEntry(entry)
	return m.revision != before
}

// Consume folds one typed activity event into the model and reports whether
// the display changed.
func (m *OperatorActivityModel) Consume(event observability.ActivityEvent) bool {
	if m == nil {
		return false
	}
	before := m.revision
	switch event.Kind {
	case observability.ActivityAgentTextDelta, observability.ActivityAgentMessage:
		m.consumeMessage(event)
	case observability.ActivityAgentReasoningDelta, observability.ActivityAgentReasoningSummary:
		m.consumeReasoning(event)
	case observability.ActivityToolStarted, observability.ActivityToolCompleted, observability.ActivityToolFailed:
		m.consumeTool(event)
	case observability.ActivityBuildStarted,
		observability.ActivityBuildCompileCompleted,
		observability.ActivityBuildCandidateStarted,
		observability.ActivityBuildCandidateReady,
		observability.ActivityBuildProtocolValidated,
		observability.ActivityBuildCandidateFailed,
		observability.ActivityBuildCompleted:
		m.consumeBuild(event)
	case observability.ActivityExitInterviewQuestion:
		if text, ok := event.Data["text"].(string); ok && strings.TrimSpace(text) != "" {
			m.upsertEntry(ActivityDisplayEntry{
				Key:          "interview-question:" + strconv.FormatUint(event.Sequence, 10),
				Kind:         ActivityDisplayKindInterviewQuestion,
				Presentation: InterviewQuestionPresentation{Text: SafeDisplayText(text, m.displayBytes)},
			})
		}
	default:
		m.consumeLifecycle(event)
	}
	return m.revision != before
}

// RenderText returns the plain transcript representation used by diagnostics
// and tests.  Frontends may provide richer rendering from the typed entries.
func (m *OperatorActivityModel) RenderText() string {
	if m == nil {
		return ""
	}
	parts := make([]string, 0, len(m.entries))
	for _, entry := range m.entries {
		parts = append(parts, entryText(entry))
	}
	return strings.Join(parts, "\n\n")
}

func (m *OperatorActivityModel) consumeMessage(event observability.ActivityEvent) {
	key := m.correlationKey(event, "message")
	text, ok := event.Data["text"].(string)
	if !ok {
		return
	}
	finalized := event.Kind == observability.ActivityAgentMessage
	turnPhase, _ := event.Data["turn_phase"].(string)
	if !finalized {
		text = m.messageText[key] + text
	}
	m.messageText[key] = text
	if event.Role == observability.ActivityReviewer {
		identity := makeTextIdentity(text)
		m.latestReviewerMessage = &identity
	}
	m.upsertEntry(ActivityDisplayEntry{
		Key:  key,
		Kind: ActivityDisplayKindMessage,
		Presentation: AgentMessagePresentation{
			Role: event.Role, Text: SafeDisplayText(text, m.displayBytes),
			Finalized: finalized, TurnPhase: turnPhase,
		},
	})
	if finalized {
		delete(m.messageText, key)
	}
}

func (m *OperatorActivityModel) consumeReasoning(event observability.ActivityEvent) {
	key := m.correlationKey(event, "reasoning")
	parts := m.reasoningText[key]
	if parts == nil {
		parts = make(map[int]string)
		m.reasoningText[key] = parts
	}
	finalized := event.Kind == observability.ActivityAgentReasoningSummary
	turnPhase, _ := event.Data["turn_phase"].(string)
	if !finalized {
		text, textOK := event.Data["text"].(string)
		index, indexOK := integerValue(event.Data["summary_index"])
		if !textOK || !indexOK {
			return
		}
		parts[index] += text
	} else {
		summary, ok := stringSlice(event.Data["summary"])
		if !ok {
			return
		}
		clear(parts)
		for index, text := range summary {
			parts[index] = text
		}
	}
	indices := make([]int, 0, len(parts))
	for index := range parts {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	textParts := make([]string, 0, len(indices))
	for _, index := range indices {
		textParts = append(textParts, parts[index])
	}
	text := strings.Join(textParts, "\n")
	if strings.TrimSpace(text) == "" {
		if finalized {
			delete(m.reasoningText, key)
			m.remove(key)
		}
		return
	}
	m.upsertEntry(ActivityDisplayEntry{
		Key:  key,
		Kind: ActivityDisplayKindReasoning,
		Presentation: ReasoningPresentation{
			Role: event.Role, Text: SafeDisplayText(text, m.displayBytes),
			Finalized: finalized, TurnPhase: turnPhase,
		},
	})
	if finalized {
		delete(m.reasoningText, key)
	}
}

func (m *OperatorActivityModel) consumeTool(event observability.ActivityEvent) {
	key := m.correlationKey(event, "tool")
	existing, hasExisting := m.toolPresentations[key]
	tool, ok := event.Data["tool"].(string)
	if !ok {
		if hasExisting {
			tool = existing.Tool
		} else {
			tool = "unknown"
		}
	}
	arguments, _ := event.Data["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	state := ActivityDisplayStateRunning
	switch event.Kind {
	case observability.ActivityToolCompleted:
		state = ActivityDisplayStateCompleted
	case observability.ActivityToolFailed:
		state = ActivityDisplayStateFailed
	}
	if tool == "request_feature" {
		m.consumeFeatureRequest(key, event, arguments)
		return
	}
	summary := toolSummary(tool, arguments)
	var existingPointer *ToolPresentation
	if hasExisting {
		copy := existing
		existingPointer = &copy
	}
	detail := m.toolDetail(tool, arguments, event.Data, existingPointer, state)
	resultNote := ""
	if result, ok := event.Data["result"].(string); ok && tool == "review" && m.latestReviewerMessage != nil {
		identity := makeTextIdentity(result)
		if identity == *m.latestReviewerMessage {
			detail = nil
			resultNote = "result returned to Astra"
		}
	}
	turnPhase, _ := event.Data["turn_phase"].(string)
	if turnPhase == "" && hasExisting {
		turnPhase = existing.TurnPhase
	}
	presentation := ToolPresentation{
		Role: event.Role, Tool: SafeDisplayText(tool, SummaryDisplayBytes), TurnPhase: turnPhase, State: state, Summary: summary,
		Detail: detail, ResultNote: resultNote,
	}
	m.toolPresentations[key] = presentation
	m.upsertEntry(ActivityDisplayEntry{Key: key, Kind: ActivityDisplayKindTool, Presentation: presentation})
	if event.Kind != observability.ActivityToolStarted {
		delete(m.toolPresentations, key)
	}
}

func (m *OperatorActivityModel) consumeFeatureRequest(key string, event observability.ActivityEvent, arguments map[string]any) {
	title, titleOK := arguments["title"].(string)
	if !titleOK {
		title = "External capability request"
	}
	description, _ := arguments["description"].(string)
	recordingState := FeatureRequestRecordingStateRecording
	switch event.Kind {
	case observability.ActivityToolCompleted:
		recordingState = FeatureRequestRecordingStateRecorded
	case observability.ActivityToolFailed:
		recordingState = FeatureRequestRecordingStateFailed
	}
	var initialStatus *FeatureRequestInitialStatus
	if recordingState == FeatureRequestRecordingStateRecorded {
		status := FeatureRequestInitialStatusPending
		initialStatus = &status
	}
	errorText := ""
	if recordingState == FeatureRequestRecordingStateFailed {
		errorText = featureRequestError(event.Data)
	}
	m.upsertEntry(ActivityDisplayEntry{
		Key: key, Kind: ActivityDisplayKindFeatureRequest,
		Presentation: FeatureRequestPresentation{
			Role: event.Role, RecordingState: recordingState,
			InitialStatus: initialStatus,
			Title:         SafeDisplayText(title, SummaryDisplayBytes),
			Description:   SafeDisplayText(description, m.displayBytes),
			RequestID:     featureRequestID(event.Data["result"]), Error: errorText,
		},
	})
}

func (m *OperatorActivityModel) toolDetail(tool string, arguments, data map[string]any, existing *ToolPresentation, state ActivityDisplayState) *ToolDetailPresentation {
	errorValue := data["error"]
	resultValue := data["result"]
	if state == ActivityDisplayStateFailed {
		if !emptyPayload(errorValue) {
			return payloadPresentation(errorValue, m.displayBytes, nil)
		}
		if !emptyPayload(resultValue) {
			return payloadPresentation(resultValue, m.displayBytes, nil)
		}
	}
	if tool == "write" {
		content, exists := arguments["data"]
		if !exists {
			content = arguments["content"]
		}
		if content != nil {
			return payloadPresentation(content, m.displayBytes, arguments["encoding"])
		}
	}
	if !emptyPayload(errorValue) {
		return payloadPresentation(errorValue, m.displayBytes, nil)
	}
	if result, ok := resultValue.(map[string]any); ok && equalsPythonZero(result["status"]) && emptyPayload(result["output"]) {
		if existing != nil {
			return existing.Detail
		}
		return nil
	}
	if !emptyPayload(resultValue) {
		return payloadPresentation(resultValue, m.displayBytes, nil)
	}
	if existing != nil {
		return existing.Detail
	}
	return nil
}

func (m *OperatorActivityModel) consumeBuild(event observability.ActivityEvent) {
	var key string
	var build BuildPresentation
	if event.Kind == observability.ActivityBuildStarted {
		m.buildNumber++
		key = buildKey(event.Generation, m.buildNumber)
		m.activeBuildKey = key
		build = newRunningBuild()
	} else {
		key = m.activeBuildKey
		if key == "" {
			m.buildNumber++
			key = buildKey(event.Generation, m.buildNumber)
			m.activeBuildKey = key
			build = newPendingBuild()
		} else {
			var ok bool
			build, ok = m.builds[key]
			if !ok {
				build = newPendingBuild()
			}
		}
		build = advanceBuild(build, event, m.displayBytes)
	}
	m.builds[key] = build
	m.upsertEntry(ActivityDisplayEntry{Key: key, Kind: ActivityDisplayKindBuild, Presentation: build})
	if event.Kind == observability.ActivityBuildCompleted {
		m.activeBuildKey = ""
	}
}

func newRunningBuild() BuildPresentation {
	return BuildPresentation{
		State: ActivityDisplayStateRunning,
		Phases: []BuildPhasePresentation{
			{Name: "compile/link", State: ActivityDisplayStateRunning},
			{Name: "candidate boot", State: ActivityDisplayStatePending},
			{Name: "READY", State: ActivityDisplayStatePending},
			{Name: "protocol", State: ActivityDisplayStatePending},
		},
	}
}

func newPendingBuild() BuildPresentation {
	build := newRunningBuild()
	for index := range build.Phases {
		build.Phases[index].State = ActivityDisplayStatePending
	}
	return build
}

func advanceBuild(build BuildPresentation, event observability.ActivityEvent, limit int) BuildPresentation {
	// Copy the slice because presentation values are treated as immutable by
	// rows that may retain an earlier snapshot.
	build.Phases = append([]BuildPhasePresentation(nil), build.Phases...)
	result, hasResult := event.Data["result"]
	if !hasResult {
		result = event.Data["status"]
	}
	switch event.Kind {
	case observability.ActivityBuildCompileCompleted:
		success := result == "success"
		if len(build.Phases) > 0 {
			if success {
				build.Phases[0].State = ActivityDisplayStateCompleted
			} else {
				build.Phases[0].State = ActivityDisplayStateFailed
			}
		}
		if !success {
			build.State = ActivityDisplayStateFailed
			build.Diagnostic = buildDiagnostic(event.Data, limit)
		}
	case observability.ActivityBuildCandidateStarted:
		if len(build.Phases) > 1 {
			build.Phases[1].State = ActivityDisplayStateRunning
		}
	case observability.ActivityBuildCandidateReady:
		if len(build.Phases) > 3 {
			build.Phases[1].State = ActivityDisplayStateCompleted
			build.Phases[2].State = ActivityDisplayStateCompleted
			build.Phases[3].State = ActivityDisplayStateRunning
		}
	case observability.ActivityBuildProtocolValidated:
		if len(build.Phases) > 3 {
			build.Phases[3].State = ActivityDisplayStateCompleted
		}
	case observability.ActivityBuildCandidateFailed:
		for _, index := range []int{1, 2, 3} {
			if index < len(build.Phases) && build.Phases[index].State != ActivityDisplayStateCompleted {
				build.Phases[index].State = ActivityDisplayStateFailed
				break
			}
		}
		build.State = ActivityDisplayStateFailed
		build.Diagnostic = buildDiagnostic(event.Data, limit)
	case observability.ActivityBuildCompleted:
		success := equalsPythonZero(result) || result == "0" || result == "success"
		if success {
			build.State = ActivityDisplayStateCompleted
		} else {
			build.State = ActivityDisplayStateFailed
			if build.Diagnostic == "" {
				build.Diagnostic = buildDiagnostic(event.Data, limit)
			}
		}
	}
	return build
}

func (m *OperatorActivityModel) consumeLifecycle(event observability.ActivityEvent) {
	switch event.Kind {
	case observability.ActivitySessionStarted,
		observability.ActivitySessionStopped,
		observability.ActivityTurnStarted,
		observability.ActivityTurnCompleted,
		observability.ActivityReviewStarted,
		observability.ActivityReviewCompleted,
		observability.ActivityExitInterviewStarted,
		observability.ActivityExitInterviewEnded:
		return
	}
	state := ActivityDisplayStateFailed
	switch event.Kind {
	case observability.ActivityTurnInterrupted:
		state = ActivityDisplayStateInterrupted
	case observability.ActivityTurnFailed, observability.ActivityReviewFailed:
		state = ActivityDisplayStateFailed
	case observability.ActivityReviewCancelled:
		state = ActivityDisplayStateCancelled
	}
	useful := make(map[string]any, len(event.Data))
	for key, value := range event.Data {
		switch key {
		case "model", "reasoning_effort", "reasoning_summary", "service_tier", "service_tier_name", "agent_contract_version":
			continue
		default:
			useful[key] = value
		}
	}
	detail := ""
	if len(useful) != 0 {
		detail = lifecycleDetail(useful, m.displayBytes)
	}
	m.upsertEntry(ActivityDisplayEntry{
		Key:          "lifecycle:" + strconv.FormatUint(event.Sequence, 10),
		Kind:         ActivityDisplayKindLifecycle,
		Presentation: LifecyclePresentation{Role: event.Role, Title: readableLifecycleTitle(event.Kind), Detail: detail, State: state},
	})
}

func readableLifecycleTitle(kind observability.ActivityKind) string {
	title := strings.NewReplacer("_", " ", ".", " ").Replace(strings.TrimSpace(string(kind)))
	return title
}

func lifecycleDetail(values map[string]any, limit int) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(left, right int) bool {
		leftError := keys[left] == "error" || keys[left] == "reason"
		rightError := keys[right] == "error" || keys[right] == "reason"
		if leftError != rightError {
			return leftError
		}
		return keys[left] < keys[right]
	})
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		appendLifecycleValue(&lines, key, values[key])
	}
	return SafeDisplayText(strings.Join(lines, "\n"), limit)
}

func appendLifecycleValue(lines *[]string, key string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for nested := range typed {
			keys = append(keys, nested)
		}
		sort.Strings(keys)
		for _, nested := range keys {
			appendLifecycleValue(lines, key+"."+nested, typed[nested])
		}
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, lifecycleScalar(item))
		}
		*lines = append(*lines, key+": "+strings.Join(parts, ", "))
	default:
		*lines = append(*lines, key+": "+lifecycleScalar(value))
	}
}

func lifecycleScalar(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.ReplaceAll(typed, "\n", " / ")
	case nil:
		return "none"
	default:
		return fmt.Sprint(typed)
	}
}

func (m *OperatorActivityModel) correlationKey(event observability.ActivityEvent, suffix string) string {
	item := event.ItemID
	if item == "" {
		item = "sequence-" + strconv.FormatUint(event.Sequence, 10)
	}
	thread := event.ThreadID
	if thread == "" {
		thread = "thread"
	}
	turn := event.TurnID
	if turn == "" {
		turn = "turn"
	}
	return strings.Join([]string{string(event.Role), thread, turn, item, suffix}, ":")
}

func (m *OperatorActivityModel) upsertEntry(entry ActivityDisplayEntry) {
	position, ok := m.positions[entry.Key]
	if !ok {
		m.positions[entry.Key] = len(m.entries)
		m.entries = append(m.entries, entry)
		m.revision++
	} else if !reflect.DeepEqual(m.entries[position], entry) {
		m.entries[position] = entry
		m.revision++
	}
	m.trim()
}

func (m *OperatorActivityModel) remove(key string) {
	position, ok := m.positions[key]
	if !ok {
		return
	}
	m.entries = append(m.entries[:position], m.entries[position+1:]...)
	m.forget(key)
	m.positions = make(map[string]int, len(m.entries))
	for index, entry := range m.entries {
		m.positions[entry.Key] = index
	}
	m.revision++
}

func (m *OperatorActivityModel) trim() {
	filtered := m.entries[:0]
	for _, entry := range m.entries {
		if entry.Key != "scrollback:discarded" {
			filtered = append(filtered, entry)
		}
	}
	m.entries = filtered
	total := 0
	for _, entry := range m.entries {
		total += entry.SizeBytes()
	}
	discarded := false
	reserveMarker := m.discarded || len(m.entries) > m.maxEntries || total > m.maxBytes
	allowedEntries := m.maxEntries
	if reserveMarker {
		allowedEntries--
	}
	for (len(m.entries) > allowedEntries || total > m.maxBytes) && len(m.entries) > 1 {
		removed := m.entries[0]
		m.entries = m.entries[1:]
		total -= removed.SizeBytes()
		m.forget(removed.Key)
		discarded = true
	}
	if discarded {
		m.discarded = true
		m.revision++
	}
	if m.discarded {
		m.entries = append([]ActivityDisplayEntry{{
			Key: "scrollback:discarded", Kind: ActivityDisplayKindNotice,
			Presentation: NoticePresentation{Title: "Harness", Text: "… older live activity discarded from UI scrollback …"},
		}}, m.entries...)
	}
	m.positions = make(map[string]int, len(m.entries))
	for index, entry := range m.entries {
		m.positions[entry.Key] = index
	}
}

func (m *OperatorActivityModel) forget(key string) {
	delete(m.messageText, key)
	delete(m.reasoningText, key)
	delete(m.toolPresentations, key)
	delete(m.builds, key)
	if m.activeBuildKey == key {
		m.activeBuildKey = ""
	}
	if m.activeOperatorKey == key {
		m.activeOperatorKey = ""
	}
}

// SafeDisplayText escapes terminal controls while retaining ordinary newlines
// and replacing tabs with four spaces.  The byte limit is applied before
// control escaping, matching the Python reference model.
func SafeDisplayText(text string, limit ...int) string {
	limitBytes := DefaultDisplayBytes
	if len(limit) > 1 {
		panic("SafeDisplayText accepts at most one byte limit")
	}
	if len(limit) == 1 {
		limitBytes = limit[0]
	}
	normalized := normalizeUTF8(text)
	encoded := []byte(normalized)
	originalSize := len(encoded)
	if limitBytes < 0 {
		limitBytes = 0
	}
	remaining := 0
	if len(encoded) > limitBytes {
		encoded = encoded[:limitBytes]
		for len(encoded) > 0 && !utf8.Valid(encoded) {
			encoded = encoded[:len(encoded)-1]
		}
		remaining = originalSize - len(encoded)
	}
	text = string(encoded)
	var escaped strings.Builder
	for _, character := range text {
		codepoint := int(character)
		switch character {
		case '\n':
			escaped.WriteByte('\n')
		case '\t':
			escaped.WriteString("    ")
		case '\r':
			escaped.WriteString(`\r`)
		default:
			if codepoint <= 0x1f || codepoint == 0x7f || (codepoint >= 0x80 && codepoint <= 0x9f) {
				escaped.WriteString(fmt.Sprintf(`\x%02x`, codepoint))
			} else {
				escaped.WriteRune(character)
			}
		}
	}
	if remaining != 0 {
		escaped.WriteString("\n… ")
		escaped.WriteString(strconv.Itoa(remaining))
		escaped.WriteString(" bytes more in activity payload …")
	}
	return escaped.String()
}

// SafeDisplayBytes renders valid UTF-8 as text and invalid UTF-8 as a stable
// binary summary.  Binary detail is intentionally bounded to about half the
// normal display budget so its prefix and byte count remain visible.
func SafeDisplayBytes(data []byte, limit ...int) string {
	limitBytes := DefaultDisplayBytes
	if len(limit) > 1 {
		panic("SafeDisplayBytes accepts at most one byte limit")
	}
	if len(limit) == 1 {
		limitBytes = limit[0]
	}
	if utf8.Valid(data) {
		return SafeDisplayText(string(data), limitBytes)
	}
	shownLimit := limitBytes / 2
	if shownLimit < 1 {
		shownLimit = 1
	}
	shown := data
	if len(shown) > shownLimit {
		shown = shown[:shownLimit]
	}
	suffix := ""
	if len(shown) < len(data) {
		suffix = fmt.Sprintf(" … %d bytes more …", len(data)-len(shown))
	}
	parts := make([]string, len(shown))
	for index, value := range shown {
		parts[index] = fmt.Sprintf("%02x", value)
	}
	return fmt.Sprintf("binary (%d bytes): %s%s", len(data), strings.Join(parts, " "), suffix)
}

func normalizeUTF8(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	var normalized strings.Builder
	for index := 0; index < len(text); {
		character, size := utf8.DecodeRuneInString(text[index:])
		if character == utf8.RuneError && size == 1 {
			normalized.WriteString(fmt.Sprintf(`\x%02x`, text[index]))
			index++
			continue
		}
		normalized.WriteRune(character)
		index += size
	}
	return normalized.String()
}

func payloadPresentation(value any, limitBytes int, encodingValue any) *ToolDetailPresentation {
	if object, ok := value.(map[string]any); ok {
		if _, exists := object["output"]; exists {
			output := object["output"]
			if !emptyPayload(output) {
				return payloadPresentation(output, limitBytes, nil)
			}
			value = "status=" + pythonString(object["status"])
		}
	}
	if data, ok := value.([]byte); ok {
		if !utf8.Valid(data) {
			return &ToolDetailPresentation{
				Text: SafeDisplayBytes(data, limitBytes), ByteCount: len(data), Binary: true,
				Truncated: len(data) > maxInt(1, limitBytes/2),
			}
		}
		text := string(data)
		lineCount := lineCount(text)
		return &ToolDetailPresentation{
			Text: SafeDisplayText(text, limitBytes), ByteCount: len(data), LineCount: &lineCount,
			Truncated: len(data) > limitBytes,
		}
	}
	if text, ok := value.(string); ok && encodingValue == "base64" {
		if strictBase64(text) {
			decoded, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				decoded = nil
			}
			if err == nil {
				return &ToolDetailPresentation{
					Text:      fmt.Sprintf("binary (%d bytes, base64): %s", len(decoded), SafeDisplayText(text, limitBytes)),
					ByteCount: len(decoded), Binary: true,
					Truncated: len([]byte(text)) > limitBytes,
				}
			}
		}
	}
	if _, ok := value.(string); !ok {
		value = pythonJSON(value)
	}
	text, _ := value.(string)
	encoded := []byte(normalizeUTF8(text))
	lineCountValue := lineCount(text)
	return &ToolDetailPresentation{
		Text: SafeDisplayText(text, limitBytes), ByteCount: len(encoded), LineCount: &lineCountValue,
		Truncated: len(encoded) > limitBytes,
	}
}

func featureRequestID(value any) string {
	object, ok := value.(map[string]any)
	if !ok || !equalsPythonZero(object["status"]) {
		return ""
	}
	output := object["output"]
	var text string
	switch output := output.(type) {
	case []byte:
		if !isASCII(output) {
			return ""
		}
		text = string(output)
	case string:
		if !isASCII([]byte(output)) {
			return ""
		}
		text = output
	default:
		return ""
	}
	if text == "" || text[0] == '0' {
		return ""
	}
	for _, character := range text {
		if character < '0' || character > '9' {
			return ""
		}
	}
	return text
}

func featureRequestError(data map[string]any) string {
	if errorValue := data["error"]; !emptyPayload(errorValue) {
		return oneLine(payloadPresentation(errorValue, SummaryDisplayBytes, nil).Text)
	}
	if result, ok := data["result"].(map[string]any); ok {
		if output := result["output"]; !emptyPayload(output) {
			return oneLine(payloadPresentation(output, SummaryDisplayBytes, nil).Text)
		}
	}
	return "request recording failed"
}

func toolSummary(tool string, arguments map[string]any) string {
	value := func(name string) (string, bool) {
		candidate, exists := arguments[name]
		if !exists {
			return "", false
		}
		if text, ok := candidate.(string); ok {
			return SafeDisplayText(text, SummaryDisplayBytes), true
		}
		if integer, ok := integerString(candidate); ok {
			return SafeDisplayText(integer, SummaryDisplayBytes), true
		}
		return "", false
	}
	path, pathOK := value("path")
	if (tool == "read" || tool == "write" || tool == "remove") && pathOK {
		extras := make([]string, 0, 2)
		if tool == "read" || tool == "write" {
			offset, offsetOK := value("offset")
			if offsetOK && offset != "0" {
				extras = append(extras, "offset="+offset)
			}
		}
		if tool == "read" {
			length, lengthOK := value("length")
			if lengthOK {
				extras = append(extras, "length="+length)
			}
		}
		if len(extras) > 0 {
			return path + "  " + strings.Join(extras, " ")
		}
		return path
	}
	if tool == "truncate" && pathOK {
		size, sizeOK := value("size")
		if !sizeOK || size == "" {
			size, sizeOK = value("length")
		}
		if !sizeOK || size == "" {
			return path
		}
		return path + " → " + size + " bytes"
	}
	if tool == "list" {
		if prefix, ok := value("prefix"); ok && prefix != "" {
			return prefix
		}
		return "guest source"
	}
	if tool == "review" {
		if focus, ok := value("focus"); ok && focus != "" {
			return focus
		}
		return "independent review"
	}
	if tool == "finish_generation" {
		return "validated successor and handoff"
	}
	if tool == "build" {
		return "exact current source"
	}
	if len(arguments) > 0 {
		return SafeDisplayText(oneLine(pythonJSON(arguments)), SummaryDisplayBytes)
	}
	return ""
}

func buildDiagnostic(data map[string]any, limit int) string {
	useful := make(map[string]any, len(data))
	for key, value := range data {
		if key == "status" && (value == nil || value == "") {
			continue
		}
		useful[key] = value
	}
	if len(useful) == 0 {
		return ""
	}
	if len(useful) == 1 {
		if result, ok := useful["result"]; ok {
			return SafeDisplayText(pythonString(result), limit)
		}
	}
	return SafeDisplayText(pythonJSON(useful), limit)
}

func buildKey(generation *uint64, number int) string {
	generationText := "None"
	if generation != nil {
		generationText = strconv.FormatUint(*generation, 10)
	}
	return "build:" + generationText + ":" + strconv.Itoa(number)
}

func entryText(entry ActivityDisplayEntry) string {
	switch presentation := entry.Presentation.(type) {
	case AgentMessagePresentation:
		phase := ""
		if presentation.TurnPhase == "planning" {
			phase = " · planning"
		}
		return roleName(presentation.Role) + phase + "\n" + presentation.Text
	case ReasoningPresentation:
		phase := ""
		if presentation.TurnPhase == "planning" {
			phase = " · planning"
		}
		return roleName(presentation.Role) + phase + " · reasoning\n" + presentation.Text
	case ToolPresentation:
		text := strings.TrimRightFunc(roleName(presentation.Role)+" · "+presentation.Tool+" "+presentation.Summary, unicode.IsSpace)
		if presentation.Detail != nil {
			text += "\n" + presentation.Detail.Text
		}
		return text
	case FeatureRequestPresentation:
		lines := []string{"Feature request", presentation.Title, presentation.Description, string(presentation.RecordingState)}
		if presentation.RequestID != "" {
			lines = append(lines, "request "+presentation.RequestID)
		}
		if presentation.InitialStatus != nil {
			lines = append(lines, "initial status: "+string(*presentation.InitialStatus), "recording did not provision the capability")
		}
		if presentation.Error != "" {
			lines = append(lines, presentation.Error)
		}
		return nonEmptyLines(lines)
	case BuildPresentation:
		lines := []string{"Trusted build"}
		for _, phase := range presentation.Phases {
			lines = append(lines, phase.Name+" "+string(phase.State))
		}
		return strings.Join(lines, "\n")
	case OperatorPresentation:
		command := ""
		if presentation.Command != nil {
			command = "codexos> " + *presentation.Command + "\n"
		}
		return "Operator\n" + command + presentation.Output
	case InterviewQuestionPresentation:
		return "You\n" + presentation.Text
	case LifecyclePresentation:
		return roleName(presentation.Role) + " · " + presentation.Title + "\n" + presentation.Detail
	case NoticePresentation:
		if presentation.Text == "" {
			return presentation.Title
		}
		return presentation.Title + "\n" + presentation.Text
	default:
		return ""
	}
}

func presentationText(presentation ActivityPresentation) string {
	switch presentation := presentation.(type) {
	case AgentMessagePresentation:
		return presentation.Text
	case ReasoningPresentation:
		return presentation.Text
	case ToolPresentation:
		parts := []string{presentation.Tool, presentation.Summary}
		if presentation.Detail != nil {
			parts = append(parts, presentation.Detail.Text)
		}
		parts = append(parts, presentation.ResultNote)
		return nonEmptyLines(parts)
	case FeatureRequestPresentation:
		status := ""
		if presentation.InitialStatus != nil {
			status = string(*presentation.InitialStatus)
		}
		return strings.Join([]string{string(presentation.RecordingState), status, presentation.Title, presentation.Description, presentation.RequestID, presentation.Error}, "\n")
	case BuildPresentation:
		parts := make([]string, 0, len(presentation.Phases)+1)
		for _, phase := range presentation.Phases {
			parts = append(parts, phase.Name+string(phase.State))
		}
		parts = append(parts, presentation.Diagnostic)
		return strings.Join(parts, "\n")
	case OperatorPresentation:
		command := ""
		if presentation.Command != nil {
			command = *presentation.Command
		}
		return command + "\n" + presentation.Output
	case InterviewQuestionPresentation:
		return presentation.Text
	case LifecyclePresentation:
		return presentation.Title + "\n" + presentation.Detail
	case NoticePresentation:
		return presentation.Title + "\n" + presentation.Text
	default:
		return ""
	}
}

func roleName(role observability.ActivityRole) string {
	switch role {
	case observability.ActivityImplementor, observability.ActivityReviewer:
		return "Astra"
	case observability.ActivityHarness:
		return "Harness"
	default:
		return string(role)
	}
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	count := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		count++
	}
	return count
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func emptyPayload(value any) bool {
	if value == nil {
		return true
	}
	switch value := value.(type) {
	case string:
		return value == ""
	case []byte:
		return len(value) == 0
	default:
		return false
	}
}

func equalsPythonZero(value any) bool {
	switch value := value.(type) {
	case int:
		return value == 0
	case int8:
		return value == 0
	case int16:
		return value == 0
	case int32:
		return value == 0
	case int64:
		return value == 0
	case uint:
		return value == 0
	case uint8:
		return value == 0
	case uint16:
		return value == 0
	case uint32:
		return value == 0
	case uint64:
		return value == 0
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		return err == nil && parsed == 0
	case float32:
		return value == 0
	case float64:
		return value == 0
	case bool:
		return !value
	default:
		return false
	}
}

func integerString(value any) (string, bool) {
	switch value := value.(type) {
	case int:
		return strconv.Itoa(value), true
	case int8:
		return strconv.FormatInt(int64(value), 10), true
	case int16:
		return strconv.FormatInt(int64(value), 10), true
	case int32:
		return strconv.FormatInt(int64(value), 10), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case uint:
		return strconv.FormatUint(uint64(value), 10), true
	case uint8:
		return strconv.FormatUint(uint64(value), 10), true
	case uint16:
		return strconv.FormatUint(uint64(value), 10), true
	case uint32:
		return strconv.FormatUint(uint64(value), 10), true
	case uint64:
		return strconv.FormatUint(value, 10), true
	case json.Number:
		if _, err := strconv.ParseInt(string(value), 10, 64); err == nil {
			return string(value), true
		}
	case bool:
		if value {
			return "True", true
		}
		return "False", true
	}
	return "", false
}

func integerValue(value any) (int, bool) {
	if boolean, ok := value.(bool); ok {
		if boolean {
			return 1, true
		}
		return 0, true
	}
	text, ok := integerString(value)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || int64(int(parsed)) != parsed {
		return 0, false
	}
	return int(parsed), true
}

func stringSlice(value any) ([]string, bool) {
	switch value := value.(type) {
	case []string:
		return append([]string(nil), value...), true
	case []any:
		result := make([]string, len(value))
		for index, item := range value {
			text, ok := item.(string)
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

func isASCII(data []byte) bool {
	for _, value := range data {
		if value > 0x7f {
			return false
		}
	}
	return true
}

func strictBase64(text string) bool {
	for _, value := range []byte(text) {
		if (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z') ||
			(value >= '0' && value <= '9') || value == '+' || value == '/' || value == '=' {
			continue
		}
		return false
	}
	return true
}

func maxInt(first, second int) int {
	if first > second {
		return first
	}
	return second
}

func pythonString(value any) string {
	if value == nil {
		return "None"
	}
	if boolean, ok := value.(bool); ok {
		if boolean {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}

func nonEmptyLines(lines []string) string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

type textIdentity struct {
	length int
	hash   [sha256.Size]byte
}

func makeTextIdentity(text string) textIdentity {
	encoded := []byte(normalizeUTF8(text))
	return textIdentity{length: len(encoded), hash: sha256.Sum256(encoded)}
}

// pythonJSON is the small deterministic JSON rendering used where the
// reference model uses json.dumps(..., ensure_ascii=False, sort_keys=True,
// default=str).  encoding/json already sorts string map keys; this wrapper
// supplies Python's readable separators and its fallback string conversion.
func pythonJSON(value any) string {
	return pythonJSONValue(value)
}

func pythonJSONValue(value any) string {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, jsonQuote(key)+": "+pythonJSONValue(value[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(value))
		for index, item := range value {
			parts[index] = pythonJSONValue(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []byte:
		return jsonQuote(pythonBytesString(value))
	case []string:
		parts := make([]string, len(value))
		for index, item := range value {
			parts[index] = jsonQuote(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case nil:
		return "null"
	case string:
		return jsonQuote(value)
	case json.Number:
		return string(value)
	case bool:
		if value {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(value)
	case int8:
		return strconv.FormatInt(int64(value), 10)
	case int16:
		return strconv.FormatInt(int64(value), 10)
	case int32:
		return strconv.FormatInt(int64(value), 10)
	case int64:
		return strconv.FormatInt(value, 10)
	case uint:
		return strconv.FormatUint(uint64(value), 10)
	case uint8:
		return strconv.FormatUint(uint64(value), 10)
	case uint16:
		return strconv.FormatUint(uint64(value), 10)
	case uint32:
		return strconv.FormatUint(uint64(value), 10)
	case uint64:
		return strconv.FormatUint(value, 10)
	case float32:
		return pythonJSONFloat(float64(value), 32)
	case float64:
		return pythonJSONFloat(value, 64)
	default:
		reflected := reflect.ValueOf(value)
		if reflected.IsValid() {
			switch reflected.Kind() {
			case reflect.Map:
				if reflected.Type().Key().Kind() == reflect.String {
					keys := reflected.MapKeys()
					sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
					parts := make([]string, 0, len(keys))
					for _, key := range keys {
						parts = append(parts, jsonQuote(key.String())+": "+pythonJSONValue(reflected.MapIndex(key).Interface()))
					}
					return "{" + strings.Join(parts, ", ") + "}"
				}
			case reflect.Slice, reflect.Array:
				parts := make([]string, reflected.Len())
				for index := range parts {
					parts[index] = pythonJSONValue(reflected.Index(index).Interface())
				}
				return "[" + strings.Join(parts, ", ") + "]"
			}
		}
		return jsonQuote(fmt.Sprint(value))
	}
}

func pythonJSONFloat(value float64, bitSize int) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	encoded := strconv.FormatFloat(value, 'g', -1, bitSize)
	if !strings.ContainsAny(encoded, ".eE") {
		encoded += ".0"
	}
	return encoded
}

func pythonBytesString(value []byte) string {
	quote := byte('\'')
	if bytes.ContainsRune(value, '\'') && !bytes.ContainsRune(value, '"') {
		quote = '"'
	}
	var output strings.Builder
	output.WriteByte('b')
	output.WriteByte(quote)
	for _, character := range value {
		switch character {
		case '\\':
			output.WriteString(`\\`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if character == quote {
				output.WriteByte('\\')
				output.WriteByte(character)
			} else if character >= 0x20 && character <= 0x7e {
				output.WriteByte(character)
			} else {
				fmt.Fprintf(&output, `\x%02x`, character)
			}
		}
	}
	output.WriteByte(quote)
	return output.String()
}

func jsonQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return strconv.Quote(normalizeUTF8(value))
	}
	// Python ensure_ascii=False leaves UTF-8 and the HTML-sensitive characters
	// alone.  json.Marshal escapes those characters by default.
	return strings.NewReplacer(
		"\\u003c", "<", "\\u003e", ">", "\\u0026", "&",
		"\\u2028", " ", "\\u2029", " ",
	).Replace(string(encoded))
}
