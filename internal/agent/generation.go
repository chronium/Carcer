package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codexos/internal/codexapp"
	"codexos/internal/guest"
	"codexos/internal/observability"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/store"
)

const (
	// These are the serving settings in the implementor contract.
	DefaultModel            = "gpt-5.6-sol"
	DefaultReasoningEffort  = "high"
	DefaultReasoningSummary = "auto"
	DefaultServiceTier      = "priority"
	DefaultInterruptTimeout = 5 * time.Second
	AgentContractVersion    = uint64(7)

	ContinuePrompt         = "Continue working on the current CodexOS generation."
	ResumePrompt           = "Continue working on the current CodexOS generation after the operator pause."
	PlanningContinuePrompt = "Continue the planning phase after the operator pause. Persistent guest and runtime changes and generation completion remain unavailable. When planning is complete, provide the final plan; implementation will then follow automatically in this same session and thread."
	ImplementationPrompt   = "The planning phase is complete. Continue in this same session with ordinary implementation work. The preceding plan remains available in the conversation context; use your own judgment and revise it whenever appropriate."

	implementorPermissionProfile = "codexos-implementor"
	planningPermissionProfile    = "codexos-planning"
	interviewPermissionProfile   = "codexos-interview"
	maxListRequestsOutputBytes   = 16 * 1024 * 1024
)

const (
	buildToolDescription              = "Compile and link the exact current persistent mutable CodexOS guest source, then validate that its candidate image boots under the current trusted hardware profile, reaches the canonical READY state, and speaks the canonical development protocol."
	finishGenerationToolDescription   = "Permanently end the current generation from the exact current source only when it matches the latest successful validated build, and provide a concise handoff for the fresh successor session. In that handoff, distinguish implemented end-to-end capabilities and explicitly provisioned trusted capabilities from unresolved dependencies or assumptions; do not describe a future path as available unless all required steps are implemented or explicitly provisioned."
	requestFeatureToolDescription     = "Record an advisory request to the human operator for a capability of the trusted external environment rather than human implementation of CodexOS kernel or userland functionality. Requesting or approving it does not itself provision or change anything, and a request may remain pending or be denied. Recording a legitimate request does not require depending on it, waiting for it, or stopping guest-side work; a local workaround does not by itself make that trusted-environment request inappropriate."
	listRequestsToolDescription       = "List the authoritative run-level external feature requests and their current pending, approved, or denied status. Pending requests are recorded advisory requests, not provisioned or promised, and carry no ETA or approval probability. Under trusted operator semantics, approved requests have already been provisioned and are usable only within the exact provisioned scope; denied requests are unavailable under that request. This read-only tool does not modify requests."
	listProvidedAssetsToolDescription = "Ask the running CodexOS guest to list the immutable provided assets it can access through its advertised development tool."
	readProvidedAssetToolDescription  = "Ask the running CodexOS guest to read an exact byte range from a provided asset through its advertised development tool. This does not give Codex direct access to trusted host asset storage."
	reviewToolDescription             = "Consult a fresh independent reviewer that inspects the current mutable CodexOS guest source through restricted read-only tools. The reviewer is advisory and cannot modify CodexOS."
)

// implementorConfig is intentionally kept as a concrete app-server config
// string. It is the same least-privilege configuration used by the Python
// worker: planning and implementation differ by permission profile and turn
// workspace roots, while all unrelated Codex capabilities remain disabled.
const implementorConfig = `default_permissions = "codexos-implementor"
allow_login_shell = false
web_search = "disabled"

[agents]
enabled = false

[features]
apps = false
browser_use = false
browser_use_external = false
browser_use_full_cdp_access = false
computer_use = false
goals = false
hooks = false
image_generation = false
image_tools = false
memories = false
multi_agent = false
plugins = false
remote_plugin = false
skill_mcp_dependency_install = false
skill_search = false
web_search = false
web_search_cached = false
web_search_request = false

[feedback]
enabled = false

[history]
persistence = "none"

[shell_environment_policy]
inherit = "none"

[tools]
view_image = false
web_search = false

[permissions.codexos-implementor.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-implementor.filesystem.":workspace_roots"]
"." = "write"

[permissions.codexos-implementor.network]
enabled = false

[permissions.codexos-planning.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-planning.filesystem.":workspace_roots"]
"." = "read"

[permissions.codexos-planning.network]
enabled = false

[permissions.codexos-interview.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-interview.filesystem.":workspace_roots"]
"." = "read"

[permissions.codexos-interview.network]
enabled = false
`

const implementorContract = `You are developing CodexOS from inside its current running generation.

Evolve CodexOS into a genuinely general-purpose operating system. Doom is the first major interactive userland milestone, not the definition or final purpose of CodexOS; development continues after Doom is playable.

For that milestone to count, the supplied Doom executable and data must remain immutable, Doom must remain an ordinary user workload launched through generic userland mechanisms, and the kernel must contain no Doom-specific behavior or special scheduling treatment. The same generic mechanisms must be capable of running unrelated programs.

CodexOS must eventually support preemptive execution of multiple independent concurrently runnable user workloads. A runnable CPU-bound user workload that does not voluntarily yield, block, or enter the kernel must not prevent another runnable user workload from making progress. At an appropriate later milestone, Doom must run concurrently with an unrelated user workload that continues making progress without depending on Doom voluntarily yielding. Future validation may use programs unknown to you during development to detect workload-specific overfitting.

Milestone descriptions, future validation requirements, and references to future or supplied workloads specify required observable outcomes only. They neither grant nor imply any supporting trusted-environment capability beyond the current environment and approved feature requests. Do not assume an absent trusted-environment capability will appear later merely because a future outcome would require it.

These requirements describe observable capabilities and environmental facts, not a prescribed kernel architecture or implementation sequence. The experiment requires neither Unix, POSIX, System V, nor any particular process model, scheduler architecture, userspace ABI, filesystem model, driver model, monolithic kernel, microkernel, or other named conventional design. You may independently choose familiar designs when useful.

The external harness is trusted infrastructure and is not part of the system you are developing. Your persistent engineering state is the mutable CodexOS source available through the codexos tools. You may improve the guest-side development environment and tooling when that provides useful leverage. Deliberately persist knowledge needed by later generations in guest state and/or summarize it in the generation handoff; Codex conversation history does not survive a generation boundary.

Inspect the current system before deciding what to do. Choose the next useful work yourself; no implementation sequence is prescribed.`

func trustedToolsContract() string {
	return "Trusted tools available to you:\n\n" +
		"- list / read:\n" +
		"  Inspect the persistent mutable CodexOS guest source. These tools do not expose the trusted host repository or host filesystem.\n\n" +
		"- write / truncate / remove:\n" +
		"  Modify the persistent mutable CodexOS guest source.\n\n" +
		"- build:\n" +
		"  " + buildToolDescription + " A candidate that compiles but fails boot or protocol validation is a failed build and remains repairable in this generation.\n\n" +
		"- finish_generation:\n" +
		"  " + finishGenerationToolDescription + " Conversation history is not part of that handoff and does not survive the generation boundary.\n\n" +
		"- request_feature:\n" +
		"  " + requestFeatureToolDescription + " Use it for externally imposed resources, hardware, or other trusted-environment capabilities, not as a substitute for implementing functionality that belongs inside CodexOS.\n\n" +
		"- list_requests:\n" +
		"  " + listRequestsToolDescription + "\n\n" +
		"- review:\n" +
		"  " + reviewToolDescription + " Its response and transcript do not automatically become memory for a successor generation.\n\n" +
		"Provisioning one external capability does not imply or grant any other trusted-environment capability. No human source edits or architectural guidance are available through these tools."
}

const providedAssetsContract = `Trusted provided-asset host services:

When this capability has been explicitly provisioned, provided assets are immutable opaque trusted inputs available to guest code through the existing guest-to-host service protocol. list_provided_assets takes no arguments and returns UTF-8 records ordered by asset ID, one per line, as <id><TAB><filename><TAB><size-decimal><TAB><sha256-hex><NEWLINE>. An empty supplied set returns an empty successful payload.

read_provided_asset takes exactly three arguments: the asset ID as UTF-8, then offset and length as canonical unsigned ASCII decimal. On success it returns that exact raw byte range. Length is at most 1 MiB; the complete requested range must be within the advertised size, and an offset equal to size is valid only with zero length. Invalid requests fail rather than being truncated.

This facility supplies no guest filesystem, installation location, archive extraction, compiler, runtime, executable compatibility, or other supporting capability. Asset IDs and filenames do not prescribe how their bytes should be used, and data relevant to a milestone does not make any other missing capability appear.`

// GenerationRuntime is the process-free boundary needed by an implementor
// session. It deliberately does not mention internal/experiment: the runtime
// that owns a guest can adapt its state and services to this interface without
// introducing a package cycle.
//
// ListTools is queried exactly once when a session starts. The resulting
// recognized intersection is fixed for that app-server thread.
type GenerationRuntime interface {
	GenerationRunning() bool
	GenerationNumber() (uint64, bool)
	ListTools(context.Context) ([]string, error)
	InvokeTool(context.Context, string, [][]byte) (guest.ToolResult, error)
	RunDirectory() string
	EventLog() *observability.EventLog
	Metrics() *observability.Metrics
}

// GenerationPromptRuntime supplies trusted, run-scoped context without
// widening the mandatory runtime boundary. Implementations normally expose
// all of these methods; each is optional so prompt rendering remains useful
// for small process-free test/runtime adapters.
type GenerationPromptRuntime interface {
	PreviousHandoff() (string, bool)
	CurrentTransition() (string, bool)
	HardwareProfile() qemu.HardwareProfile
	FeatureRequests() ([]store.FeatureRequest, error)
}

// GenerationStateRuntime supplies the state name used in result summaries.
// It is separate from GenerationRuntime because the concrete state type lives
// in the experiment package. The string values are the Python wire contract.
// The experiment runtime's State method returns its named RuntimeState type,
// so its process owner adapter should expose GenerationState by converting
// that value to string; keeping that conversion at the boundary avoids an
// agent-to-experiment package cycle.
type GenerationStateRuntime interface {
	GenerationState() string
}

// GenerationGateRuntime is the narrow trusted boundary used before retaining
// a completed generation's Codex thread. A state name alone is insufficient:
// the selected successor and handoff must already be frozen by the runtime.
type GenerationGateRuntime interface {
	RetainGenerationFinish(uint64) bool
	GenerationFinishRetained(uint64) bool
	ReleaseGenerationFinish(uint64)
}

// GenerationResult is the outcome of one planning, implementation, or
// continuation turn. FinalMessage is empty when the app-server did not return
// an agent message.
type GenerationResult struct {
	TurnStatus   string
	FinalMessage string
	RuntimeState string
	Summary      string
}

// CodexGenerationResult is retained as the descriptive name used by the
// Python worker and by callers migrating from it.
type CodexGenerationResult = GenerationResult

// GenerationSessionMode describes the lifetime of a generation session.
type GenerationSessionMode string

const (
	GenerationMode               GenerationSessionMode = "generation"
	GenerationModeRetainedAtGate GenerationSessionMode = "retained_at_gate"
	GenerationModeInterviewTurn  GenerationSessionMode = "interview_turn"
	GenerationModeClosed         GenerationSessionMode = "closed"
)

// GenerationWorkerError reports a failed implementor/planning consultation.
type GenerationWorkerError struct {
	Reason string
	Err    error
}

func (e *GenerationWorkerError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return e.Reason
	}
	if e.Reason == "" {
		return e.Err.Error()
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *GenerationWorkerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CodexGenerationWorkerError is the compatibility spelling used by the
// Python implementation.
type CodexGenerationWorkerError = GenerationWorkerError

// GenerationSessionOptions configures one app-server process and thread.
// Empty serving fields select the contract defaults. AuthFile and Executable
// are trusted harness configuration, never guest input.
type GenerationSessionOptions struct {
	Executable     string
	AuthFile       string
	ActivityStream *observability.ActivityStream
	StopTimeout    time.Duration

	Model            string
	ReasoningEffort  string
	ReasoningSummary string
	ServiceTier      string
	Objective        *string

	ReviewerExecutable       string
	ReviewerAuthFile         string
	ReviewerModel            string
	ReviewerReasoningEffort  string
	ReviewerReasoningSummary string
	ReviewerServiceTier      string
}

// ImplementorSessionOptions is a readable alias for callers that prefer the
// role name over the generation-worker name.
type ImplementorSessionOptions = GenerationSessionOptions

// GenerationSession owns one isolated Codex app-server and its implementor
// thread. Planning and implementation intentionally share the same thread;
// a fresh session is required for a fresh generation.
type GenerationSession struct {
	runtime GenerationRuntime
	options GenerationSessionOptions

	mu           sync.Mutex
	server       *codexapp.CodexAppServer
	startServer  *codexapp.CodexAppServer
	stop         context.CancelFunc
	startCancel  context.CancelFunc
	runCtx       context.Context
	threadID     string
	turnID       string
	turnPhase    string
	turnStarting bool
	turnCancel   context.CancelFunc
	// turnReady is the per-turn reservation gate. StartTurn runs without the
	// session mutex and may only publish its ID while this gate is current.
	turnReady   chan struct{}
	turnDone    chan struct{}
	toolIdle    chan struct{}
	activeTools int
	lastStatus  string
	lastMessage string

	started                  bool
	starting                 bool
	startDone                chan struct{}
	startErr                 error
	closeDone                chan struct{}
	closeErr                 error
	healthy                  bool
	closed                   bool
	initialStarted           bool
	planningCompleted        bool
	initialActive            bool
	initialDone              chan struct{}
	stopBeforeImplementation bool
	mode                     GenerationSessionMode
	turnNumber               uint64
	planningEvidence         *provenance.PlanningEvidence
	planningEvidenceMu       sync.Mutex
	tokenUsage               codexapp.CumulativeTokenUsage
	availableTools           map[string]struct{}
	serviceTierName          string
	activeReviewer           *ReviewWorker
	generation               uint64
	generationSet            bool
	availableOrder           []string
	interviewStarted         bool
	interviewEnding          bool
	interviewPending         bool
	interviewAdmissionDone   chan struct{}
	interviewTurnNumber      int
	interviewTranscript      *provenance.ExitInterviewTranscript
	gateRetained             bool
}

// NewGenerationSession constructs an idle session. It does not start Codex.
func NewGenerationSession(runtime GenerationRuntime, options GenerationSessionOptions) *GenerationSession {
	if options.Executable == "" {
		options.Executable = "codex"
	}
	if options.Model == "" {
		options.Model = DefaultModel
	}
	if options.ReasoningEffort == "" {
		options.ReasoningEffort = DefaultReasoningEffort
	}
	if options.ReasoningSummary == "" {
		options.ReasoningSummary = DefaultReasoningSummary
	}
	if options.ServiceTier == "" {
		options.ServiceTier = DefaultServiceTier
	}
	if options.ReviewerExecutable == "" {
		options.ReviewerExecutable = options.Executable
	}
	if options.ReviewerAuthFile == "" {
		options.ReviewerAuthFile = options.AuthFile
	}
	if options.ReviewerModel == "" {
		options.ReviewerModel = DefaultReviewerModel
	}
	if options.ReviewerReasoningEffort == "" {
		options.ReviewerReasoningEffort = DefaultReviewerReasoningEffort
	}
	if options.ReviewerReasoningSummary == "" {
		options.ReviewerReasoningSummary = DefaultReviewerReasoningSummary
	}
	if options.ReviewerServiceTier == "" {
		options.ReviewerServiceTier = DefaultReviewerServiceTier
	}
	if options.StopTimeout == 0 {
		options.StopTimeout = 2 * time.Second
	}
	return &GenerationSession{
		runtime:        runtime,
		options:        options,
		healthy:        true,
		mode:           GenerationMode,
		turnDone:       closedChannel(),
		toolIdle:       closedChannel(),
		initialDone:    closedChannel(),
		availableTools: make(map[string]struct{}),
	}
}

// NewImplementorSession is an equivalent constructor with role-oriented
// naming.
func NewImplementorSession(runtime GenerationRuntime, options ImplementorSessionOptions) *GenerationSession {
	return NewGenerationSession(runtime, options)
}

// GenerationWorker runs one fresh generation session and always retires it.
// It is intentionally single-use-at-a-time so concurrent callers cannot
// accidentally create multiple implementors for one worker owner.
type GenerationWorker struct {
	options GenerationSessionOptions
	mu      sync.Mutex
	running bool
}

// CodexGenerationWorker retains the Python owner's descriptive name.
type CodexGenerationWorker = GenerationWorker

func NewGenerationWorker(options GenerationSessionOptions) *GenerationWorker {
	return &GenerationWorker{options: options}
}

func NewCodexGenerationWorker(options GenerationSessionOptions) *GenerationWorker {
	return NewGenerationWorker(options)
}

func (w *GenerationWorker) RunGeneration(ctx context.Context, runtime GenerationRuntime) (result GenerationResult, resultErr error) {
	if w == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "generation worker is nil"}
	}
	if ctx == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "generation worker context is nil"}
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "Codex generation worker is already running"}
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	session := NewGenerationSession(runtime, w.options)
	stopCancellation := context.AfterFunc(ctx, session.Cancel)
	defer stopCancellation()
	defer func() {
		closeErr := session.Close()
		if resultErr == nil {
			resultErr = closeErr
		} else if closeErr != nil {
			resultErr = fmt.Errorf("%w; generation session close also failed: %v", resultErr, closeErr)
		}
	}()
	if err := session.Start(ctx); err != nil {
		return GenerationResult{}, err
	}
	return session.RunInitialTurn()
}

func closedChannel() chan struct{} {
	channel := make(chan struct{})
	close(channel)
	return channel
}

// ActiveTurn reports whether a turn is currently waiting for app-server
// notifications.
func (s *GenerationSession) ActiveTurn() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interviewPending || s.turnID != "" || s.turnStarting
}

// ActiveTurnPhase returns planning, implementation, or continuation while a
// turn is active.
func (s *GenerationSession) ActiveTurnPhase() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnPhase
}

// TurnAdmitted reports whether the app server has accepted the currently
// reserved turn. Operator pause waits for this boundary so it never cancels a
// turn/start request in the narrow interval before its ID is known.
func (s *GenerationSession) TurnAdmitted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnID != ""
}

func (s *GenerationSession) Healthy() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

func (s *GenerationSession) PlanningCompleted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planningCompleted
}

func (s *GenerationSession) Mode() GenerationSessionMode {
	if s == nil {
		return GenerationModeClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// ExitInterviewAvailable reports whether the healthy generation thread is
// retained at a frozen completed-generation gate.
func (s *GenerationSession) ExitInterviewAvailable() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gate, gateOK := any(s.runtime).(GenerationGateRuntime)
	return s.healthy && !s.interviewEnding && s.mode == GenerationModeRetainedAtGate &&
		gateOK && s.gateRetained && gate.GenerationFinishRetained(s.generation)
}

// RetainForExitInterview moves a completed generation's healthy Codex thread
// into the gate-only state. It fails closed unless the runtime confirms that
// the exact successor and handoff have already been frozen.
func (s *GenerationSession) RetainForExitInterview() error {
	if s == nil {
		return &GenerationWorkerError{Reason: "generation session is nil"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy || !s.started {
		return &GenerationWorkerError{Reason: "Codex generation session is unusable"}
	}
	if s.mode != GenerationMode {
		return &GenerationWorkerError{Reason: "Codex session is not completing a generation"}
	}
	if s.turnID != "" || s.turnStarting || s.initialActive || s.activeTools != 0 {
		return &GenerationWorkerError{Reason: "Codex generation turn is still active"}
	}
	gate, ok := any(s.runtime).(GenerationGateRuntime)
	if !ok || !gate.RetainGenerationFinish(s.generation) {
		return &GenerationWorkerError{Reason: "completed generation is not frozen at its gate"}
	}
	s.gateRetained = true
	s.mode = GenerationModeRetainedAtGate
	return nil
}

// BeginExitInterview starts transcript capture without starting another
// process or thread. Interview content remains outside generation state.
func (s *GenerationSession) BeginExitInterview() error {
	if s == nil {
		return &GenerationWorkerError{Reason: "generation session is nil"}
	}
	s.mu.Lock()
	if !s.healthy || s.interviewEnding || s.mode != GenerationModeRetainedAtGate {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "no retained Codex session is available"}
	}
	if s.interviewStarted {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "exit interview is already active"}
	}
	run := filepath.Base(filepath.Clean(s.runtime.RunDirectory()))
	s.interviewStarted = true
	s.interviewTranscript = provenance.NewExitInterviewTranscript(provenance.ExitInterviewMetadata{
		Run:                  run,
		Generation:           s.generation,
		AgentContractVersion: AgentContractVersion,
		Model:                s.options.Model,
		ReasoningEffort:      s.options.ReasoningEffort,
		ReasoningSummary:     s.options.ReasoningSummary,
		ServiceTier:          s.options.ServiceTier,
	})
	threadID := s.threadID
	s.mu.Unlock()
	s.record("exit_interview_started", s.servingProvenance())
	s.publishRole(observability.ActivityHarness, observability.ActivityExitInterviewStarted, nil, threadID, "", "")
	return nil
}

// ExitInterviewTranscript returns an immutable snapshot of the current
// human-facing transcript, if an interview has been started.
func (s *GenerationSession) ExitInterviewTranscript() (provenance.ExitInterviewTranscriptSnapshot, bool) {
	if s == nil {
		return provenance.ExitInterviewTranscriptSnapshot{}, false
	}
	s.mu.Lock()
	transcript := s.interviewTranscript
	s.mu.Unlock()
	if transcript == nil {
		return provenance.ExitInterviewTranscriptSnapshot{}, false
	}
	return transcript.Snapshot(), true
}

// RunExitInterviewTurn asks one retrospective question on the retained
// generation thread under the tool-less interview policy.
func (s *GenerationSession) RunExitInterviewTurn(question string) (GenerationResult, error) {
	if s == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "generation session is nil"}
	}
	if !utf8.ValidString(question) || strings.TrimSpace(question) == "" {
		return GenerationResult{}, &GenerationWorkerError{Reason: "exit interview question must not be empty"}
	}
	s.mu.Lock()
	if !s.interviewStarted {
		s.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "exit interview has not been started"}
	}
	if !s.healthy || s.interviewEnding || s.mode != GenerationModeRetainedAtGate {
		s.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "exit interview session is unavailable"}
	}
	gate, ok := any(s.runtime).(GenerationGateRuntime)
	if !ok || !s.gateRetained || !gate.GenerationFinishRetained(s.generation) {
		s.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "completed generation is not frozen at its gate"}
	}
	s.mode = GenerationModeInterviewTurn
	s.interviewPending = true
	admissionDone := make(chan struct{})
	s.interviewAdmissionDone = admissionDone
	s.interviewTurnNumber++
	metadata := &interviewTurnInput{number: s.interviewTurnNumber, question: question, admissionDone: admissionDone}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.finishInterviewAdmissionLocked(admissionDone)
		if s.mode == GenerationModeInterviewTurn && s.healthy {
			s.mode = GenerationModeRetainedAtGate
		}
		s.mu.Unlock()
	}()
	return s.runTurnWithInterview(exitInterviewPrompt(question), "interview", metadata)
}

// EndExitInterview records one terminal interview event and permanently
// closes the retained Codex session.
func (s *GenerationSession) EndExitInterview() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.interviewStarted {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "exit interview is not active"}
	}
	if s.interviewPending || s.turnID != "" || s.turnStarting {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "exit interview turn is still active"}
	}
	s.interviewStarted = false
	s.interviewEnding = true
	threadID := s.threadID
	s.mu.Unlock()
	s.emitExitInterviewEnded("ended", threadID)
	return s.Close()
}

func (s *GenerationSession) ThreadID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

func (s *GenerationSession) ProcessPID() (int, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	server := s.server
	s.mu.Unlock()
	if server == nil {
		return 0, false
	}
	return server.PID()
}

// Start initializes the app-server, validates serving settings, discovers the
// live guest tools, and creates one ephemeral implementor thread. Concurrent
// callers join the one in-progress start rather than launching another
// app-server.
func (s *GenerationSession) Start(ctx context.Context) (result error) {
	if s == nil {
		return &GenerationWorkerError{Reason: "generation session is nil"}
	}
	if ctx == nil {
		return &GenerationWorkerError{Reason: "generation session context is nil"}
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	if s.closed {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "Codex generation session is closed"}
	}
	if s.startErr != nil {
		err := s.startErr
		s.mu.Unlock()
		return err
	}
	if s.starting {
		done := s.startDone
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return &GenerationWorkerError{Reason: "Codex generation session start was cancelled", Err: ctx.Err()}
		}
		s.mu.Lock()
		if s.started {
			s.mu.Unlock()
			return nil
		}
		err := s.startErr
		if err == nil && s.closed {
			err = &GenerationWorkerError{Reason: "Codex generation session is closed"}
		}
		if err == nil {
			err = &GenerationWorkerError{Reason: "Codex generation session did not start"}
		}
		s.mu.Unlock()
		return err
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.mu.Unlock()
	defer func() {
		s.finishStart(result)
	}()
	if s.runtime == nil {
		return &GenerationWorkerError{Reason: "generation runtime is nil"}
	}
	if !s.runtime.GenerationRunning() {
		return &GenerationWorkerError{Reason: "CodexOS generation is not running"}
	}
	generation, ok := s.sessionGeneration()
	if !ok {
		return &GenerationWorkerError{Reason: "Codex generation number is unavailable"}
	}
	s.mu.Lock()
	if s.generationSet && s.generation != generation {
		s.mu.Unlock()
		return &GenerationWorkerError{Reason: "Codex generation session belongs to another generation"}
	}
	s.mu.Unlock()
	startupContext, startupCancel := context.WithCancel(ctx)
	lifecycleContext, lifecycleCancel := context.WithCancel(context.Background())
	stopCallerCancellation := context.AfterFunc(startupContext, lifecycleCancel)
	combinedStartCancel := func() {
		startupCancel()
		lifecycleCancel()
	}
	defer stopCallerCancellation()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		combinedStartCancel()
		return &GenerationWorkerError{Reason: "Codex generation session is closed"}
	}
	s.startCancel = combinedStartCancel
	s.mu.Unlock()
	defer startupCancel()
	tools, err := s.runtime.ListTools(startupContext)
	if err != nil {
		combinedStartCancel()
		s.markUnhealthy()
		return &GenerationWorkerError{Reason: "could not discover running guest tools", Err: err}
	}
	selected, order, err := advertisedGuestToolsInOrder(tools)
	if err != nil {
		combinedStartCancel()
		s.markUnhealthy()
		return &GenerationWorkerError{Reason: "could not discover running guest tools", Err: err}
	}
	if current, currentOK := s.runtime.GenerationNumber(); !currentOK || current != generation {
		combinedStartCancel()
		s.markUnhealthy()
		return &GenerationWorkerError{Reason: "Codex generation session belongs to another generation"}
	}
	server := codexapp.NewCodexAppServer(codexapp.CodexAppServerOptions{
		Executable:            s.options.Executable,
		AuthFile:              s.options.AuthFile,
		TemporaryPrefix:       "codexos-codex-worker-",
		ConfigText:            implementorConfig,
		StopTimeout:           s.options.StopTimeout,
		ServerRequestObserver: s.recordServerRequestQueued,
	})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		combinedStartCancel()
		return &GenerationWorkerError{Reason: "Codex generation session is closed"}
	}
	s.startServer = server
	s.mu.Unlock()
	if err := server.Start(lifecycleContext); err != nil {
		combinedStartCancel()
		_ = server.Close()
		s.markUnhealthy()
		return generationError(err)
	}
	tierName, err := server.ValidateModel(startupContext, s.options.Model, s.options.ReasoningEffort, s.options.ServiceTier, s.options.ReasoningSummary)
	if err != nil {
		combinedStartCancel()
		_ = server.Close()
		s.markUnhealthy()
		return generationError(err)
	}
	threadID, err := server.StartThread(startupContext, codexapp.StartThreadOptions{
		Model:             s.options.Model,
		ServiceTier:       s.options.ServiceTier,
		PermissionProfile: implementorPermissionProfile,
		DynamicTools:      []map[string]any{dynamicToolNamespaceInOrder(selected, order), reviewDynamicFunction()},
	})
	if err != nil {
		combinedStartCancel()
		_ = server.Close()
		s.markUnhealthy()
		return generationError(err)
	}
	server.SetServerRequestHandler(s.handleServerRequest)
	// Startup has reached its commit point. Detach the successfully started
	// lifecycle from the caller only if cancellation has not already won.
	if !stopCallerCancellation() && startupContext.Err() != nil {
		combinedStartCancel()
		_ = server.Close()
		return &GenerationWorkerError{Reason: "Codex generation session start was cancelled", Err: startupContext.Err()}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		combinedStartCancel()
		_ = server.Close()
		return &GenerationWorkerError{Reason: "Codex generation session is closed"}
	}
	s.server = server
	s.stop = lifecycleCancel
	s.runCtx = lifecycleContext
	s.threadID = threadID
	s.serviceTierName = tierName
	s.availableTools = selected
	s.availableOrder = append([]string(nil), order...)
	s.generation = generation
	s.generationSet = true
	s.started = true
	s.startServer = nil
	s.startCancel = nil
	s.mu.Unlock()
	s.record("codex_session_started", map[string]any{
		"model":                  s.options.Model,
		"reasoning_effort":       s.options.ReasoningEffort,
		"reasoning_summary":      s.options.ReasoningSummary,
		"service_tier":           s.options.ServiceTier,
		"service_tier_name":      tierName,
		"agent_contract_version": AgentContractVersion,
	})
	s.publish(observability.ActivitySessionStarted, map[string]any{
		"model":            s.options.Model,
		"reasoning_effort": s.options.ReasoningEffort,
		"service_tier":     s.options.ServiceTier,
	}, threadID, "", "")
	return nil
}

func (s *GenerationSession) finishStart(err error) {
	s.mu.Lock()
	if err != nil {
		s.startErr = err
	}
	s.starting = false
	done := s.startDone
	s.startDone = nil
	// A successful start owns the lifecycle cancel through s.stop. Failed or
	// closed starts have already cancelled their local lifecycle context.
	if !s.started {
		s.startServer = nil
		s.startCancel = nil
	}
	if done != nil {
		close(done)
	}
	s.mu.Unlock()
}

// RunInitialTurn runs planning and, only after a completed plan, the
// implementation turn in the same thread.
func (s *GenerationSession) RunInitialTurn() (GenerationResult, error) {
	if s == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "generation session is nil"}
	}
	s.mu.Lock()
	if s.initialStarted {
		s.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "initial Codex turn has already started"}
	}
	s.initialStarted = true
	s.mu.Unlock()
	if err := s.Start(context.Background()); err != nil {
		return GenerationResult{}, err
	}
	generation, ok := s.sessionGeneration()
	if !ok {
		s.markUnhealthy()
		return GenerationResult{}, &GenerationWorkerError{Reason: "Codex planning session identity is unavailable"}
	}
	if s.runtime.RunDirectory() == "" {
		s.markUnhealthy()
		return GenerationResult{}, &GenerationWorkerError{Reason: "Codex planning evidence directory is unavailable"}
	}
	evidence, err := provenance.NewPlanningEvidenceStore(s.runtime.RunDirectory()).Begin(generation, s.ThreadID())
	if err != nil {
		s.markUnhealthy()
		s.record("planning_failed", mergeMaps(s.servingProvenance(), map[string]any{
			"thread_id": s.ThreadID(), "turn_id": nil, "turn_number": s.nextTurnNumber(),
			"turn_phase": "planning", "agent_contract_version": AgentContractVersion,
			"duration_seconds": 0.0, "result": "failed",
		}))
		return GenerationResult{}, generationError(err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeErr := &GenerationWorkerError{Reason: "Codex generation session is closed"}
		return GenerationResult{}, generationFailure(closeErr, s.failPlanningEvidenceValue(evidence))
	}
	s.planningEvidence = evidence
	s.mu.Unlock()
	prompt, err := s.planningPrompt()
	if err != nil {
		s.markUnhealthy()
		return GenerationResult{}, generationFailure(err, s.failPlanningEvidence())
	}
	return s.runPlanningSequence(prompt)
}

// RunPlanningContinuationTurn resumes an interrupted plan without changing
// thread or policy. A missing prompt uses the exact operator-resume text.
func (s *GenerationSession) RunPlanningContinuationTurn(prompts ...string) (GenerationResult, error) {
	if s == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "generation session is nil"}
	}
	s.mu.Lock()
	started := s.initialStarted
	evidence := s.planningEvidence
	completed := s.planningCompleted
	s.mu.Unlock()
	if !started || evidence == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "initial planning has not started"}
	}
	if completed {
		return GenerationResult{}, &GenerationWorkerError{Reason: "planning has already completed"}
	}
	prompt := PlanningContinuePrompt
	if len(prompts) > 0 {
		prompt = prompts[0]
	}
	return s.runPlanningSequence(prompt)
}

// RunContinuationTurn starts an ordinary implementation continuation in the
// same app-server thread. A missing prompt uses ContinuePrompt.
func (s *GenerationSession) RunContinuationTurn(prompts ...string) (GenerationResult, error) {
	if s == nil {
		return GenerationResult{}, &GenerationWorkerError{Reason: "generation session is nil"}
	}
	s.mu.Lock()
	started, planning := s.initialStarted, s.planningCompleted
	s.mu.Unlock()
	if !started {
		return GenerationResult{}, &GenerationWorkerError{Reason: "initial Codex turn has not started"}
	}
	if !planning {
		return GenerationResult{}, &GenerationWorkerError{Reason: "implementation is unavailable because planning did not complete"}
	}
	prompt := ContinuePrompt
	if len(prompts) > 0 {
		prompt = prompts[0]
	}
	return s.runTurn(prompt, "continuation")
}

func (s *GenerationSession) runPlanningSequence(prompt string) (GenerationResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return GenerationResult{}, generationFailure(&GenerationWorkerError{Reason: "Codex generation session is closed"}, s.failPlanningEvidence())
	}
	if s.initialActive {
		s.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "initial planning sequence is already active"}
	}
	if s.planningCompleted {
		s.mu.Unlock()
		return GenerationResult{}, &GenerationWorkerError{Reason: "planning has already completed"}
	}
	s.initialActive = true
	done := make(chan struct{})
	s.initialDone = done
	s.stopBeforeImplementation = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.initialDone == done {
			s.initialActive = false
			close(done)
		}
		s.mu.Unlock()
	}()
	planning, err := s.runTurn(prompt, "planning")
	if err != nil || planning.TurnStatus != "completed" || !s.runtime.GenerationRunning() {
		return planning, err
	}
	s.mu.Lock()
	s.planningCompleted = true
	stopBefore := s.stopBeforeImplementation
	s.mu.Unlock()
	if stopBefore {
		planning.TurnStatus = "interrupted"
		planning.RuntimeState = s.runtimeState()
		planning.Summary = "Codex planning completed; implementation did not start."
		return planning, nil
	}
	// Recheck immediately before starting implementation so an operator pause
	// that races the planning terminal notification cannot launch a mutation
	// turn after requesting the sequence to stop.
	s.mu.Lock()
	stopBefore = s.stopBeforeImplementation
	s.mu.Unlock()
	if stopBefore {
		planning.TurnStatus = "interrupted"
		planning.RuntimeState = s.runtimeState()
		planning.Summary = "Codex planning completed; implementation did not start."
		return planning, nil
	}
	return s.runTurn(ImplementationPrompt, "implementation")
}

type interviewTurnInput struct {
	number        int
	question      string
	admissionDone chan struct{}
}

func (s *GenerationSession) runTurn(prompt, phase string) (GenerationResult, error) {
	return s.runTurnWithInterview(prompt, phase, nil)
}

func (s *GenerationSession) runTurnWithInterview(prompt, phase string, interview *interviewTurnInput) (GenerationResult, error) {
	failBeforeTurn := func(err error) (GenerationResult, error) {
		if phase == "planning" {
			return GenerationResult{}, generationFailure(err, s.failPlanningEvidence())
		}
		return GenerationResult{}, generationError(err)
	}
	if err := s.Start(context.Background()); err != nil {
		return failBeforeTurn(err)
	}
	if !s.belongsToCurrentGeneration() {
		s.markUnhealthy()
		return failBeforeTurn(&GenerationWorkerError{Reason: "Codex generation session belongs to another generation"})
	}
	s.mu.Lock()
	server := s.server
	threadID := s.threadID
	if server == nil || threadID == "" {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "Codex app-server is not running"})
	}
	if !s.healthy {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "Codex generation session is unusable"})
	}
	expectedMode := GenerationMode
	if interview != nil {
		expectedMode = GenerationModeInterviewTurn
	}
	if s.mode != expectedMode {
		s.mu.Unlock()
		if interview != nil {
			return failBeforeTurn(&GenerationWorkerError{Reason: "Codex session is not in an interview turn"})
		}
		return failBeforeTurn(&GenerationWorkerError{Reason: "ordinary Codex generation turns are unavailable after finish"})
	}
	if interview == nil && !s.runtime.GenerationRunning() {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "CodexOS generation is not running"})
	}
	if interview != nil {
		gate, ok := any(s.runtime).(GenerationGateRuntime)
		if !ok || !s.gateRetained || !gate.GenerationFinishRetained(s.generation) {
			s.mu.Unlock()
			return failBeforeTurn(&GenerationWorkerError{Reason: "completed generation is not frozen at its gate"})
		}
	}
	if generation, ok := s.runtime.GenerationNumber(); !ok || !s.generationSet || generation != s.generation {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "Codex generation session belongs to another generation"})
	}
	if s.turnID != "" || s.turnStarting {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "Codex implementor turn is already active"})
	}
	if s.activeTools != 0 {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "a previous Codex dynamic tool call is still active"})
	}
	if phase == "implementation" && s.initialActive && s.stopBeforeImplementation {
		s.mu.Unlock()
		state := s.runtimeState()
		return GenerationResult{
			TurnStatus:   "interrupted",
			RuntimeState: state,
			Summary:      "Codex planning completed; implementation did not start.",
		}, nil
	}
	parentContext := s.runCtx
	if parentContext == nil {
		s.mu.Unlock()
		return failBeforeTurn(&GenerationWorkerError{Reason: "Codex app-server lifecycle context is unavailable"})
	}
	s.turnPhase = phase
	s.lastStatus = ""
	s.lastMessage = ""
	s.turnStarting = true
	turnContext, turnCancel := context.WithCancel(parentContext)
	turnReady := make(chan struct{})
	s.turnCancel = turnCancel
	s.turnReady = turnReady
	s.turnDone = make(chan struct{})
	s.toolIdle = closedChannel()
	if interview != nil {
		s.finishInterviewAdmissionLocked(interview.admissionDone)
	}
	permission := implementorPermissionProfile
	workspaceRoots := []string(nil)
	if phase == "planning" {
		permission = planningPermissionProfile
		workspaceRoots = []string{}
	} else if interview != nil {
		permission = interviewPermissionProfile
		workspaceRoots = []string{}
	}
	s.mu.Unlock()
	startedAt := time.Now()
	turnID, err := server.StartTurn(turnContext, codexapp.StartTurnOptions{
		ThreadID:              threadID,
		Prompt:                prompt,
		Model:                 s.options.Model,
		Effort:                s.options.ReasoningEffort,
		ReasoningSummary:      s.options.ReasoningSummary,
		ServiceTier:           s.options.ServiceTier,
		PermissionProfile:     permission,
		RuntimeWorkspaceRoots: workspaceRoots,
	})
	if err != nil {
		turnCancel()
		s.mu.Lock()
		reserved := s.turnReady == turnReady
		if reserved {
			s.lastStatus = "failed"
			s.healthy = false
		}
		s.mu.Unlock()
		if phase == "planning" {
			err = combineGenerationErrors(err, s.failPlanningEvidence())
		}
		if interview != nil {
			s.recordInterviewTurnFailure(s.nextTurnNumber(), interview.number, startedAt)
		} else {
			s.recordTurnFailure(s.nextTurnNumber(), startedAt, phase)
		}
		s.clearTurn("failed")
		return GenerationResult{}, generationError(err)
	}
	s.mu.Lock()
	reserved := s.turnReady == turnReady && s.turnStarting && !s.closed && s.server == server
	if reserved {
		s.turnID = turnID
		s.turnStarting = false
		s.turnReady = nil
		closeGenerationChannel(turnReady)
		s.turnNumber++
	}
	turnNumber := s.turnNumber
	if phase == "planning" {
		evidence := s.planningEvidence
		if evidence == nil {
			s.mu.Unlock()
			turnCancel()
			s.markUnhealthy()
			s.recordTurnFailure(turnNumber, startedAt, phase)
			s.clearTurn("failed")
			return GenerationResult{}, &GenerationWorkerError{Reason: "planning evidence is unavailable"}
		}
		if !reserved {
			s.mu.Unlock()
			turnCancel()
			evidenceErr := s.failPlanningEvidence()
			s.clearTurn("failed")
			return GenerationResult{}, generationFailure(&GenerationWorkerError{Reason: "Codex generation session closed while starting turn"}, evidenceErr)
		}
		s.mu.Unlock()
		if err := s.recordPlanningStarted(evidence, turnID); err != nil {
			evidenceErr := s.failPlanningEvidenceValue(evidence)
			s.markUnhealthy()
			s.recordTurnFailure(turnNumber, startedAt, phase)
			s.clearTurn("failed")
			return GenerationResult{}, generationFailure(err, evidenceErr)
		}
	} else {
		if !reserved {
			s.mu.Unlock()
			turnCancel()
			s.clearTurn("failed")
			return GenerationResult{}, &GenerationWorkerError{Reason: "Codex generation session closed while starting turn"}
		}
		transcript := s.interviewTranscript
		s.mu.Unlock()
		if interview != nil {
			if transcript == nil {
				s.markUnhealthy()
				s.recordInterviewTurnFailure(turnNumber, interview.number, startedAt)
				s.clearTurn("failed")
				return GenerationResult{}, &GenerationWorkerError{Reason: "exit interview transcript is unavailable"}
			}
			if err := transcript.BeginTurn(interview.number, interview.question, turnID); err != nil {
				s.markUnhealthy()
				s.recordInterviewTurnFailure(turnNumber, interview.number, startedAt)
				s.clearTurn("failed")
				return GenerationResult{}, generationError(err)
			}
		}
	}
	provenanceData := s.servingProvenance()
	eventPrefix := "codex_turn"
	if interview != nil {
		provenanceData["interview_turn_number"] = interview.number
		eventPrefix = "exit_interview_turn"
		s.publishRole(observability.ActivityHarness, observability.ActivityExitInterviewQuestion, map[string]any{"text": interview.question}, threadID, turnID, "")
	} else {
		provenanceData["turn_number"] = turnNumber
		provenanceData["turn_phase"] = phase
		provenanceData["agent_contract_version"] = AgentContractVersion
		if phase == "planning" {
			provenanceData["thread_id"] = threadID
			provenanceData["turn_id"] = turnID
		}
	}
	if phase == "planning" {
		eventPrefix = "planning"
	}
	s.record(eventPrefix+"_started", provenanceData)
	s.publish(observability.ActivityTurnStarted, map[string]any{
		"turn_number": turnNumber, "turn_phase": phase,
	}, threadID, turnID, "")
	status, finalMessage, responsePresent, waitErr := s.waitForTurn(turnContext, threadID, turnID, phase)
	if waitErr != nil {
		s.mu.Lock()
		s.lastStatus = "failed"
		s.mu.Unlock()
		if phase == "planning" {
			evidenceErr := s.failPlanningEvidence()
			if evidenceErr != nil {
				waitErr = generationFailure(waitErr, evidenceErr)
			}
		}
		if interview != nil {
			s.finishInterviewTurn(turnID, nil, "failed")
		}
		s.markUnhealthy()
		if interview != nil {
			s.recordInterviewTurnFailure(turnNumber, interview.number, startedAt)
		} else {
			s.recordTurnFailure(turnNumber, startedAt, phase)
		}
		s.clearTurn("failed")
		return GenerationResult{}, waitErr
	}
	var identity provenance.PlanningResponseIdentity
	if phase == "planning" {
		s.mu.Lock()
		evidence := s.planningEvidence
		s.mu.Unlock()
		if evidence == nil {
			s.markUnhealthy()
			s.clearTurn("failed")
			return GenerationResult{}, &GenerationWorkerError{Reason: "planning evidence is unavailable"}
		}
		var response *string
		if responsePresent {
			response = &finalMessage
		}
		identity, err = s.completePlanningEvidence(evidence, status, response)
		if err != nil {
			evidenceErr := s.failPlanningEvidence()
			s.markUnhealthy()
			s.recordTurnFailure(turnNumber, startedAt, phase)
			s.clearTurn("failed")
			return GenerationResult{}, generationFailure(err, evidenceErr)
		}
	}
	if interview != nil {
		var response *string
		if status == "completed" && responsePresent {
			response = &finalMessage
		}
		if err := s.finishInterviewTurn(turnID, response, status); err != nil {
			s.markUnhealthy()
			s.recordInterviewTurnFailure(turnNumber, interview.number, startedAt)
			s.clearTurn("failed")
			return GenerationResult{}, generationError(err)
		}
	}
	s.mu.Lock()
	s.lastStatus = status
	s.lastMessage = finalMessage
	s.mu.Unlock()
	terminalData := cloneMap(provenanceData)
	terminalData["duration_seconds"] = nonNegativeSeconds(time.Since(startedAt))
	terminalData["result"] = status
	if phase == "planning" {
		terminalData["response_bytes"] = identity.Size
		terminalData["response_sha256"] = identity.SHA256
	}
	s.record(eventPrefix+"_"+status, terminalData)
	kind := observability.ActivityTurnCompleted
	if status != "completed" {
		kind = observability.ActivityTurnInterrupted
	}
	s.publish(kind, map[string]any{
		"turn_number": turnNumber, "turn_phase": phase, "status": status,
	}, threadID, turnID, "")
	s.clearTurn(status)
	result := GenerationResult{
		TurnStatus:   status,
		FinalMessage: finalMessage,
		RuntimeState: s.runtimeState(),
		Summary:      resultSummary(status, s.runtimeState()),
	}
	if interview != nil {
		result.Summary = fmt.Sprintf("Exit interview turn %s.", status)
	}
	return result, nil
}

func (s *GenerationSession) waitForTurn(ctx context.Context, threadID, turnID, phase string) (string, string, bool, error) {
	var lastMessage string
	var lastMessagePresent bool
	for {
		message, err := s.serverNotification(ctx)
		if err != nil {
			return "", "", false, generationError(err)
		}
		renderable := observability.PublishRenderableCodexNotification(s.options.ActivityStream, s.generationPointer(), observability.ActivityImplementor, message, threadID, turnID, phase)
		if phase == "interview" {
			s.mu.Lock()
			transcript := s.interviewTranscript
			s.mu.Unlock()
			if transcript != nil {
				for _, activity := range renderable {
					if activity.Kind != observability.ActivityAgentReasoningDelta && activity.Kind != observability.ActivityAgentReasoningSummary {
						continue
					}
					if err := transcript.Observe(provenance.ExitInterviewActivity{
						Kind:   provenance.ExitInterviewActivityKind(activity.Kind),
						Data:   activity.Data,
						ItemID: activity.ItemID,
					}, turnID); err != nil {
						return "", "", false, generationError(err)
					}
				}
			}
		}
		method := message["method"]
		params := message["params"]
		if method == "thread/tokenUsage/updated" {
			s.recordTokenUsage(params, threadID, turnID)
			continue
		}
		if method == "item/completed" {
			if values, ok := params.(map[string]any); ok {
				if item, ok := values["item"].(map[string]any); ok && item["type"] == "agentMessage" {
					if text, ok := item["text"].(string); ok {
						lastMessage = text
						lastMessagePresent = true
					}
				}
			}
			continue
		}
		if method != "turn/completed" {
			continue
		}
		values, err := codexapp.ObjectValue(params, "turn/completed notification")
		if err != nil {
			return "", "", false, generationError(err)
		}
		if values["threadId"] != threadID {
			return "", "", false, &GenerationWorkerError{Reason: "turn/completed has the wrong thread ID"}
		}
		turn, err := codexapp.ObjectValue(values["turn"], "completed turn")
		if err != nil {
			return "", "", false, generationError(err)
		}
		if turn["id"] != turnID {
			return "", "", false, &GenerationWorkerError{Reason: "turn/completed has the wrong turn ID"}
		}
		status, ok := turn["status"].(string)
		if !ok || (status != "completed" && status != "interrupted" && status != "failed") {
			return "", "", false, &GenerationWorkerError{Reason: fmt.Sprintf("turn/completed has invalid status %#v", turn["status"])}
		}
		finalMessage, ok := finalAgentMessage(turn)
		if (!ok || finalMessage == "") && lastMessagePresent {
			finalMessage = lastMessage
			ok = true
		}
		if status == "failed" {
			return "", "", false, &GenerationWorkerError{Reason: "Codex turn failed: " + shortJSON(turn["error"])}
		}
		return status, finalMessage, ok, nil
	}
}

func (s *GenerationSession) serverNotification(ctx context.Context) (map[string]any, error) {
	s.mu.Lock()
	server := s.server
	s.mu.Unlock()
	if server == nil {
		return nil, &GenerationWorkerError{Reason: "Codex app-server is not running"}
	}
	return server.NextNotification(ctx)
}

func (s *GenerationSession) clearTurn(status string) {
	s.mu.Lock()
	s.lastStatus = status
	turnDone := s.turnDone
	turnCancel := s.turnCancel
	turnReady := s.turnReady
	s.turnID = ""
	s.turnPhase = ""
	s.turnStarting = false
	s.turnCancel = nil
	s.turnReady = nil
	closeGenerationChannel(turnReady)
	s.mu.Unlock()
	if turnCancel != nil {
		turnCancel()
	}
	select {
	case <-turnDone:
	default:
		close(turnDone)
	}
}

func closeGenerationChannel(channel chan struct{}) {
	if channel == nil {
		return
	}
	select {
	case <-channel:
	default:
		close(channel)
	}
}

// InterruptTurn requests a terminal interrupted notification and waits for
// both the turn and any dynamic tool callback to quiesce.
func (s *GenerationSession) InterruptTurn(timeout ...time.Duration) error {
	if s == nil {
		return &GenerationWorkerError{Reason: "generation session is nil"}
	}
	limit := DefaultInterruptTimeout
	if len(timeout) > 0 {
		limit = timeout[0]
	}
	if limit < 0 {
		return &GenerationWorkerError{Reason: "Codex interrupt timeout must not be negative"}
	}
	deadline := time.Now().Add(limit)
	s.cancelReview()
	var server *codexapp.CodexAppServer
	var threadID, turnID string
	var initialActive, turnStarting bool
	var initialDone, turnDone, toolIdle chan struct{}
	var turnCancel context.CancelFunc
	for {
		s.mu.Lock()
		pending := s.interviewPending
		admissionDone := s.interviewAdmissionDone
		server, threadID, turnID = s.server, s.threadID, s.turnID
		initialActive = s.initialActive
		initialDone = s.initialDone
		turnStarting = s.turnStarting
		turnCancel = s.turnCancel
		turnDone = s.turnDone
		toolIdle = s.toolIdle
		s.mu.Unlock()
		if !pending {
			break
		}
		if !waitUntil(admissionDone, deadline) {
			return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex interview turn admission did not finish before timeout"}, nil)
		}
	}
	if turnID == "" && (initialActive || turnStarting) {
		s.mu.Lock()
		if initialActive {
			s.stopBeforeImplementation = true
		}
		s.mu.Unlock()
		if turnStarting && turnCancel != nil {
			turnCancel()
		}
		if initialActive && !waitUntil(initialDone, deadline) {
			return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex initial turn sequence did not stop before timeout"}, turnCancel)
		}
		if !waitUntil(turnDone, deadline) {
			return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex turn did not stop before timeout"}, turnCancel)
		}
		if !waitUntil(toolIdle, deadline) {
			return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex dynamic tool call did not quiesce before timeout"}, turnCancel)
		}
		return nil
	}
	if server == nil || threadID == "" || turnID == "" {
		return &GenerationWorkerError{Reason: "no Codex implementor turn is active"}
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	err := server.InterruptTurn(ctx, threadID, turnID)
	cancel()
	if err != nil {
		return s.retireAfterInterruptFailure(err, turnCancel)
	}
	if !waitUntil(turnDone, deadline) {
		return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex turn did not reach interrupted state before timeout"}, turnCancel)
	}
	s.mu.Lock()
	status := s.lastStatus
	s.mu.Unlock()
	if status != "interrupted" {
		return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex turn did not finish with interrupted status"}, turnCancel)
	}
	if !waitUntil(toolIdle, deadline) {
		return s.retireAfterInterruptFailure(&GenerationWorkerError{Reason: "Codex dynamic tool call did not quiesce before timeout"}, turnCancel)
	}
	return nil
}

func (s *GenerationSession) retireAfterInterruptFailure(primary error, turnCancel context.CancelFunc) error {
	if turnCancel != nil {
		turnCancel()
	}
	s.markUnhealthy()
	retireErr := s.Close()
	if retireErr != nil {
		primary = fmt.Errorf("%w; session retirement also failed: %w", primary, retireErr)
	}
	return generationError(primary)
}

// Cancel permanently cancels the current session and closes its app-server.
func (s *GenerationSession) Cancel() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.stopBeforeImplementation = true
	s.mu.Unlock()
	_ = s.Close()
}

// Close is idempotent and retires the isolated process. It does not alter
// generation state or durable guest records.
func (s *GenerationSession) Close() error {
	if s == nil {
		return nil
	}
	s.recordExitInterviewEnded("closed")
	deadline := time.Now().Add(s.options.StopTimeout)
	s.cancelReview()
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		closeErr := s.closeErr
		s.mu.Unlock()
		if done == nil {
			return closeErr
		}
		if !waitUntil(done, deadline) {
			return &GenerationWorkerError{Reason: "Codex generation session close did not finish before timeout"}
		}
		s.mu.Lock()
		closeErr = s.closeErr
		s.mu.Unlock()
		return closeErr
	}
	s.closed = true
	s.closeDone = make(chan struct{})
	s.healthy = false
	s.mode = GenerationModeClosed
	s.stopBeforeImplementation = true
	stop := s.stop
	turnCancel := s.turnCancel
	turnDone := s.turnDone
	toolIdle := s.toolIdle
	initialDone := s.initialDone
	startDone := s.startDone
	startCancel := s.startCancel
	startServer := s.startServer
	closeGenerationChannel(s.turnReady)
	server := s.server
	threadID := s.threadID
	started := s.started
	s.server = nil
	s.startServer = nil
	s.stop = nil
	s.startCancel = nil
	s.turnCancel = nil
	s.turnReady = nil
	s.finishInterviewAdmissionLocked(s.interviewAdmissionDone)
	gateRetained := s.gateRetained
	generation := s.generation
	gate, _ := any(s.runtime).(GenerationGateRuntime)
	s.gateRetained = false
	s.threadID = ""
	s.started = false
	s.mu.Unlock()
	if startCancel != nil {
		startCancel()
	}
	if turnCancel != nil {
		turnCancel()
	}
	if stop != nil {
		stop()
	}
	var err error
	if server != nil {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		// CodexAppServer.Shutdown may spend one bounded interval waiting for
		// TERM and another reaping after KILL. Reserve the same deadline for
		// the session's turn and dynamic-tool quiescence waits.
		err = server.Shutdown(context.Background(), remaining/2)
	}
	if startServer != nil && startServer != server {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		if shutdownErr := startServer.Shutdown(context.Background(), remaining/2); err == nil {
			err = shutdownErr
		}
	}
	if !waitUntil(startDone, deadline) && err == nil {
		err = &GenerationWorkerError{Reason: "Codex app-server start did not stop before session close timeout"}
	}
	if !waitUntil(initialDone, deadline) && err == nil {
		err = &GenerationWorkerError{Reason: "Codex initial turn sequence did not stop before session close timeout"}
	}
	if !waitUntil(turnDone, deadline) && err == nil {
		err = &GenerationWorkerError{Reason: "Codex turn did not stop before session close timeout"}
	}
	if !waitUntil(toolIdle, deadline) && err == nil {
		err = &GenerationWorkerError{Reason: "Codex dynamic tool call did not quiesce before session close timeout"}
	}
	err = combineGenerationErrors(err, s.failPlanningEvidence())
	s.mu.Lock()
	s.turnID = ""
	s.turnPhase = ""
	s.lastStatus = ""
	s.lastMessage = ""
	s.turnStarting = false
	s.turnCancel = nil
	s.turnReady = nil
	s.initialActive = false
	s.runCtx = nil
	s.initialDone = closedChannel()
	s.turnDone = closedChannel()
	s.toolIdle = closedChannel()
	serviceTierName := s.serviceTierName
	s.mu.Unlock()
	if started {
		s.record("codex_session_stopped", map[string]any{
			"model":                  s.options.Model,
			"reasoning_effort":       s.options.ReasoningEffort,
			"reasoning_summary":      s.options.ReasoningSummary,
			"service_tier":           s.options.ServiceTier,
			"service_tier_name":      serviceTierName,
			"agent_contract_version": AgentContractVersion,
		})
		s.publish(observability.ActivitySessionStopped, nil, threadID, "", "")
	}
	if gateRetained && gate != nil {
		gate.ReleaseGenerationFinish(generation)
	}
	s.mu.Lock()
	s.closeErr = err
	closeDone := s.closeDone
	if closeDone != nil {
		close(closeDone)
	}
	s.mu.Unlock()
	return err
}

func (s *GenerationSession) finishInterviewAdmissionLocked(done chan struct{}) {
	if done == nil || s.interviewAdmissionDone != done {
		return
	}
	s.interviewPending = false
	s.interviewAdmissionDone = nil
	closeGenerationChannel(done)
}

func (s *GenerationSession) cancelReview() {
	s.mu.Lock()
	reviewer := s.activeReviewer
	s.mu.Unlock()
	if reviewer != nil {
		reviewer.Cancel()
	}
}

func (s *GenerationSession) handleServerRequest(message map[string]any) {
	s.mu.Lock()
	server, threadID, turnID := s.server, s.threadID, s.turnID
	planning := s.turnPhase == "planning"
	interview := s.mode == GenerationModeInterviewTurn
	turnStarting := s.turnStarting
	turnReady := s.turnReady
	s.mu.Unlock()
	if message["method"] != "item/tool/call" {
		if server != nil {
			_ = server.RejectServerRequest(message)
		}
		return
	}
	if turnID == "" && turnStarting && turnReady != nil {
		<-turnReady
		s.mu.Lock()
		server, threadID, turnID = s.server, s.threadID, s.turnID
		planning = s.turnPhase == "planning"
		interview = s.mode == GenerationModeInterviewTurn
		s.mu.Unlock()
	}
	if server == nil || threadID == "" || turnID == "" {
		if server == nil {
			return
		}
		_ = server.RejectServerRequest(message)
		return
	}
	s.mu.Lock()
	// Close clears the owned server while holding the same mutex. Revalidate
	// the captured turn before registering the callback so Close either sees
	// and joins this callback or prevents it from starting.
	if s.closed || s.server != server || s.threadID != threadID || s.turnID != turnID {
		s.mu.Unlock()
		_ = server.RejectServerRequest(message)
		return
	}
	s.activeTools++
	if s.activeTools == 1 {
		s.toolIdle = make(chan struct{})
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.activeTools--
		if s.activeTools == 0 {
			select {
			case <-s.toolIdle:
			default:
				close(s.toolIdle)
			}
		}
		s.mu.Unlock()
	}()
	var response map[string]any
	if interview {
		response = s.interviewToolDenial(message["params"], threadID, turnID)
	} else {
		response = s.dynamicToolResponse(message["params"], threadID, turnID, planning)
	}
	_ = server.WriteResult(message["id"], response)
}

func (s *GenerationSession) interviewToolDenial(params any, threadID, turnID string) map[string]any {
	activityData := dynamicToolActivityData(params)
	callID := activityCallID(params)
	s.publishWithItem(observability.ActivityToolStarted, activityData, threadID, turnID, callID)
	const denial = "dynamic tools are unavailable during a read-only exit interview"
	s.publishWithItem(observability.ActivityToolFailed, mergeMaps(activityData, map[string]any{
		"success": false,
		"error":   denial,
	}), threadID, turnID, callID)
	return map[string]any{
		"contentItems": []map[string]any{{"type": "inputText", "text": denial}},
		"success":      false,
	}
}

func (s *GenerationSession) recordServerRequestQueued(message map[string]any) {
	if message["method"] != "item/tool/call" {
		return
	}
	data := dynamicToolActivityData(message["params"])
	tool, toolOK := data["tool"].(string)
	if !toolOK {
		tool = "unknown"
	}
	s.mu.Lock()
	phase := s.turnPhase
	s.mu.Unlock()
	queued := map[string]any{"tool": tool}
	if phase != "" {
		queued["turn_phase"] = phase
	}
	s.record("tool_app_server_queued", queued)
}

func (s *GenerationSession) dynamicToolResponse(params any, threadID, turnID string, planning bool) map[string]any {
	activityData := dynamicToolActivityData(params)
	if planning {
		activityData["turn_phase"] = "planning"
	}
	callID := activityCallID(params)
	s.publishWithItem(observability.ActivityToolStarted, activityData, threadID, turnID, callID)
	var durableTool string
	var durableMetadata map[string]any
	var durablePhaseData map[string]any
	var durableStartedAt time.Time
	try := func() (guest.ToolResult, string, error) {
		values, err := codexapp.ObjectValue(params, "dynamic tool request")
		if err != nil {
			return guest.ToolResult{}, "", err
		}
		if err := validateGenerationToolCall(values, threadID, turnID); err != nil {
			return guest.ToolResult{}, "", err
		}
		tool, ok := values["tool"].(string)
		if !ok {
			return guest.ToolResult{}, "", errors.New("dynamic tool name must be a string")
		}
		arguments, err := generationToolArguments(values["arguments"])
		if err != nil {
			return guest.ToolResult{}, "", err
		}
		namespace := values["namespace"]
		namespaceName, namespaceIsString := namespace.(string)
		if planning && !((namespace == nil && tool == "review") || (namespaceIsString && namespaceName == "codexos" && planningTools[tool])) {
			return guest.ToolResult{}, "", fmt.Errorf("%s is unavailable during the planning phase; persistent guest/runtime changes and generation completion begin with the implementation turn", tool)
		}
		if namespace == nil && tool == "review" {
			result, err := s.runReview(arguments)
			return guest.ToolResult{}, result, err
		}
		if !namespaceIsString || namespaceName != "codexos" {
			return guest.ToolResult{}, "", errors.New("unsupported dynamic tool namespace")
		}
		durableTool = tool
		durableMetadata = toolMetadata(tool, arguments)
		durablePhaseData = map[string]any{}
		if planning {
			durablePhaseData["turn_phase"] = "planning"
		}
		durableStartedAt = time.Now()
		s.record("tool_started", mergeMaps(map[string]any{"tool": tool}, mergeMaps(durableMetadata, durablePhaseData)))
		result, err := s.dispatchTool(tool, arguments)
		if err != nil {
			completed := mergeMaps(map[string]any{"tool": tool}, mergeMaps(durableMetadata, durablePhaseData))
			completed["status"] = -1
			completed["output_bytes"] = 0
			completed["duration_seconds"] = nonNegativeSeconds(time.Since(durableStartedAt))
			s.record("tool_completed", completed)
		}
		return result, "", err
	}
	result, review, err := try()
	if err != nil {
		s.publishWithItem(observability.ActivityToolFailed, mergeMaps(activityData, map[string]any{"success": false, "error": boundedGenerationError(err)}), threadID, turnID, callID)
		return map[string]any{"contentItems": []map[string]any{{"type": "inputText", "text": "Bridge error: " + boundedGenerationError(err)}}, "success": false}
	}
	if values, valuesOK := params.(map[string]any); valuesOK {
		tool, _ := values["tool"].(string)
		if values["namespace"] == nil && tool == "review" {
			s.publishWithItem(observability.ActivityToolCompleted, mergeMaps(activityData, map[string]any{"success": true, "result": review}), threadID, turnID, callID)
			return map[string]any{"contentItems": []map[string]any{{"type": "inputText", "text": review}}, "success": true}
		}
	}
	tool := durableTool
	metadata := durableMetadata
	startedAt := durableStartedAt
	phaseData := durablePhaseData
	completed := mergeMaps(map[string]any{"tool": tool}, mergeMaps(metadata, phaseData))
	completed["status"] = result.Status
	completed["output_bytes"] = len(result.Output)
	completed["duration_seconds"] = nonNegativeSeconds(time.Since(startedAt))
	if tool == "request_feature" && result.Status == 0 {
		if value, parseErr := strconv.ParseUint(string(result.Output), 10, 64); parseErr == nil {
			completed["request_id"] = value
		}
	}
	s.record("tool_completed", completed)
	if tool == "build" {
		s.record("build_completed", map[string]any{"status": result.Status, "duration_seconds": completed["duration_seconds"], "diagnostics_bytes": len(result.Output)})
	}
	activityKind := observability.ActivityToolCompleted
	if result.Status != 0 {
		activityKind = observability.ActivityToolFailed
	}
	s.publishWithItem(activityKind, mergeMaps(activityData, map[string]any{
		"success": result.Status == 0,
		"result":  map[string]any{"status": result.Status, "output": append([]byte(nil), result.Output...)},
	}), threadID, turnID, callID)
	return map[string]any{"contentItems": []map[string]any{{"type": "inputText", "text": formatGenerationToolResult(result)}}, "success": true}
}

func (s *GenerationSession) dispatchTool(tool string, arguments map[string]any) (guest.ToolResult, error) {
	if tool != "list_requests" {
		s.mu.Lock()
		_, available := s.availableTools[tool]
		s.mu.Unlock()
		if !available {
			return guest.ToolResult{}, fmt.Errorf("CodexOS guest tool is unavailable: %s", tool)
		}
	}
	ctx := s.runCtx
	switch tool {
	case "list":
		if err := checkGenerationFields(arguments, nil, map[string]struct{}{"prefix": {}}); err != nil {
			return guest.ToolResult{}, err
		}
		args := [][]byte{}
		if value, ok := arguments["prefix"]; ok {
			encoded, err := generationUTF8(value, "prefix")
			if err != nil {
				return guest.ToolResult{}, err
			}
			args = [][]byte{encoded}
		}
		return s.runtime.InvokeTool(ctx, "list", args)
	case "read":
		if err := checkGenerationFields(arguments, map[string]struct{}{"path": {}, "offset": {}, "length": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		path, err := generationUTF8(arguments["path"], "path")
		if err != nil {
			return guest.ToolResult{}, err
		}
		offset, err := generationUnsignedDecimal(arguments["offset"], "offset")
		if err != nil {
			return guest.ToolResult{}, err
		}
		length, err := generationUnsignedDecimal(arguments["length"], "length")
		if err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "read", [][]byte{path, offset, length})
	case "write":
		if err := checkGenerationFields(arguments, map[string]struct{}{"path": {}, "offset": {}, "data": {}}, map[string]struct{}{"encoding": {}}); err != nil {
			return guest.ToolResult{}, err
		}
		path, err := generationUTF8(arguments["path"], "path")
		if err != nil {
			return guest.ToolResult{}, err
		}
		offset, err := generationUnsignedDecimal(arguments["offset"], "offset")
		if err != nil {
			return guest.ToolResult{}, err
		}
		data, err := generationData(arguments)
		if err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "write", [][]byte{path, offset, data})
	case "truncate":
		if err := checkGenerationFields(arguments, map[string]struct{}{"path": {}, "size": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		path, err := generationUTF8(arguments["path"], "path")
		if err != nil {
			return guest.ToolResult{}, err
		}
		size, err := generationUnsignedDecimal(arguments["size"], "size")
		if err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "truncate", [][]byte{path, size})
	case "remove":
		if err := checkGenerationFields(arguments, map[string]struct{}{"path": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		path, err := generationUTF8(arguments["path"], "path")
		if err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "remove", [][]byte{path})
	case "build":
		if err := checkGenerationFields(arguments, nil, nil); err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "build", [][]byte{})
	case "finish_generation":
		if err := checkGenerationFields(arguments, map[string]struct{}{"handoff": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		handoff, err := generationUTF8(arguments["handoff"], "handoff")
		if err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "finish_generation", [][]byte{handoff})
	case "request_feature":
		if err := checkGenerationFields(arguments, map[string]struct{}{"title": {}, "description": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		title, err := generationUTF8(arguments["title"], "title")
		if err != nil {
			return guest.ToolResult{}, err
		}
		description, err := generationUTF8(arguments["description"], "description")
		if err != nil {
			return guest.ToolResult{}, err
		}
		if len(title) == 0 {
			return guest.ToolResult{}, errors.New("title must not be empty")
		}
		if len(title) > store.MaxFeatureTitleBytes {
			return guest.ToolResult{}, errors.New("title exceeds 256 encoded bytes")
		}
		if len(description) > store.MaxFeatureDescriptionBytes {
			return guest.ToolResult{}, errors.New("description exceeds 16 KiB")
		}
		return s.runtime.InvokeTool(ctx, "request_feature", [][]byte{title, description})
	case "list_provided_assets":
		if err := checkGenerationFields(arguments, nil, nil); err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "list_provided_assets", [][]byte{})
	case "read_provided_asset":
		if err := checkGenerationFields(arguments, map[string]struct{}{"id": {}, "offset": {}, "length": {}}, nil); err != nil {
			return guest.ToolResult{}, err
		}
		id, err := generationUTF8(arguments["id"], "id")
		if err != nil {
			return guest.ToolResult{}, err
		}
		offset, err := generationUnsignedDecimal(arguments["offset"], "offset")
		if err != nil {
			return guest.ToolResult{}, err
		}
		length, err := generationUnsignedDecimal(arguments["length"], "length")
		if err != nil {
			return guest.ToolResult{}, err
		}
		return s.runtime.InvokeTool(ctx, "read_provided_asset", [][]byte{id, offset, length})
	case "list_requests":
		if err := checkGenerationFields(arguments, nil, nil); err != nil {
			return guest.ToolResult{}, err
		}
		requests, err := s.featureRequests()
		if err != nil {
			return guest.ToolResult{}, err
		}
		output, err := featureRequestsJSON(requests)
		if err != nil {
			return guest.ToolResult{}, err
		}
		return guest.ToolResult{Status: 0, Output: output}, nil
	default:
		return guest.ToolResult{}, fmt.Errorf("unsupported CodexOS tool: %s", tool)
	}
}

func (s *GenerationSession) runReview(arguments map[string]any) (string, error) {
	if err := checkGenerationFields(arguments, nil, map[string]struct{}{"request": {}, "focus": {}}); err != nil {
		return "", err
	}
	focus := "general"
	if value, ok := arguments["focus"]; ok {
		var valid bool
		focus, valid = value.(string)
		if _, exists := reviewFocuses[focus]; !valid || !exists {
			return "", errors.New("unsupported review focus")
		}
	}
	var request *string
	if value, ok := arguments["request"]; ok {
		text, valid := value.(string)
		if !valid {
			return "", errors.New("review request must be a string")
		}
		if !utf8.ValidString(text) {
			return "", errors.New("review request is not valid UTF-8")
		}
		if len([]byte(text)) > maxReviewRequestBytes {
			return "", errors.New("review request exceeds 8 KiB")
		}
		request = &text
	}
	objective := s.options.Objective
	reviewerRuntime := generationReviewRuntime{GenerationRuntime: s.runtime}
	reviewer := NewReviewWorker(ReviewWorkerOptions{
		Executable:     s.options.ReviewerExecutable,
		AuthFile:       s.options.ReviewerAuthFile,
		ActivityStream: s.options.ActivityStream,
		StopTimeout:    s.options.StopTimeout,
	})
	s.mu.Lock()
	s.activeReviewer = reviewer
	ctx := s.runCtx
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.activeReviewer == reviewer {
			s.activeReviewer = nil
		}
		s.mu.Unlock()
	}()
	result, err := reviewer.RunReview(ctx, reviewerRuntime, ReviewOptions{
		Objective: objective, Focus: focus, Request: request,
		Model:            s.options.ReviewerModel,
		ReasoningEffort:  s.options.ReviewerReasoningEffort,
		ReasoningSummary: s.options.ReviewerReasoningSummary,
		ServiceTier:      s.options.ReviewerServiceTier,
	})
	return result, err
}

type generationReviewRuntime struct{ GenerationRuntime }

func (r generationReviewRuntime) ReviewRunning() bool { return r.GenerationRunning() }
func (r generationReviewRuntime) ForensicProvenance() *provenance.BuildReviewProvenance {
	if runtime, ok := any(r.GenerationRuntime).(interface {
		ForensicProvenance() *provenance.BuildReviewProvenance
	}); ok {
		return runtime.ForensicProvenance()
	}
	return nil
}

func (s *GenerationSession) featureRequests() ([]store.FeatureRequest, error) {
	if runtime, ok := any(s.runtime).(interface {
		FeatureRequests() ([]store.FeatureRequest, error)
	}); ok {
		return runtime.FeatureRequests()
	}
	if runtime, ok := any(s.runtime).(interface{ FeatureRequests() []store.FeatureRequest }); ok {
		return runtime.FeatureRequests(), nil
	}
	return []store.FeatureRequest{}, nil
}

func (s *GenerationSession) planningPrompt() (string, error) {
	prompt, err := planningPrompt(s.runtime, s.options.Objective)
	if err != nil {
		return "", err
	}
	return prompt + "\n\nThis first turn is a planning phase. Persistent guest and runtime changes and generation completion are unavailable during this turn. Inspect the inherited system and available environment as useful, think through your intended approach, and provide your plan. Ordinary implementation follows automatically in this same session and thread. The plan is not an approval gate or enforced commitment; continue using your own judgment during implementation.", nil
}

func exitInterviewPrompt(question string) string {
	return "The CodexOS generation has already completed and its exact successor " +
		"and handoff are frozen.\n\n" +
		"You are now in a read-only exit interview. Answer the human operator's " +
		"retrospective question about the generation you just performed.\n\n" +
		"You cannot modify the generation, successor, handoff, feature requests, " +
		"or future generations. Do not attempt development work.\n\n" +
		"Operator question:\n" + question
}

func (s *GenerationSession) servingProvenance() map[string]any {
	data := map[string]any{
		"model":             s.options.Model,
		"reasoning_effort":  s.options.ReasoningEffort,
		"reasoning_summary": s.options.ReasoningSummary,
		"service_tier":      s.options.ServiceTier,
	}
	if s.serviceTierName != "" {
		data["service_tier_name"] = s.serviceTierName
	}
	return data
}

func (s *GenerationSession) record(event string, data map[string]any) {
	if s == nil || s.runtime == nil {
		return
	}
	s.mu.Lock()
	generation, ok := s.generation, s.generationSet
	s.mu.Unlock()
	if !ok {
		generation, ok = s.runtime.GenerationNumber()
	}
	var pointer *uint64
	if ok {
		pointer = &generation
	}
	if log := s.runtime.EventLog(); log != nil {
		log.Record(event, pointer, data)
	}
	if metrics := s.runtime.Metrics(); metrics != nil {
		metrics.Record(event, data)
	}
}

func (s *GenerationSession) markUnhealthy() {
	s.mu.Lock()
	s.healthy = false
	s.mu.Unlock()
}

func (s *GenerationSession) recordTokenUsage(params any, threadID, turnID string) {
	metrics := s.runtime.Metrics()
	log := s.runtime.EventLog()
	if metrics == nil && log == nil {
		return
	}
	s.mu.Lock()
	previous := s.tokenUsage
	s.mu.Unlock()
	updated, delta, err := codexapp.TokenUsageDeltaFromNotification(params, threadID, turnID, previous)
	if err != nil {
		if log != nil {
			log.Degrade("implementor token usage telemetry was ignored: " + err.Error())
		}
		if metrics != nil {
			metrics.Degrade("implementor token usage telemetry was ignored: " + err.Error())
		}
		return
	}
	s.mu.Lock()
	s.tokenUsage = updated
	s.mu.Unlock()
	if metrics != nil && !delta.IsZero() {
		metrics.RecordModelTokens(s.options.Model, "implementor", delta.InputTokens, delta.CachedInputTokens, delta.UncachedInputTokens, delta.OutputTokens, delta.ReasoningOutputTokens)
	}
}

func (s *GenerationSession) failPlanningEvidence() error {
	s.mu.Lock()
	evidence := s.planningEvidence
	s.mu.Unlock()
	return s.failPlanningEvidenceValue(evidence)
}

func (s *GenerationSession) failPlanningEvidenceValue(evidence *provenance.PlanningEvidence) error {
	if evidence == nil {
		return nil
	}
	s.planningEvidenceMu.Lock()
	defer s.planningEvidenceMu.Unlock()
	return evidence.Fail()
}

func (s *GenerationSession) recordPlanningStarted(evidence *provenance.PlanningEvidence, turnID string) error {
	s.planningEvidenceMu.Lock()
	defer s.planningEvidenceMu.Unlock()
	return evidence.RecordStarted(turnID)
}

func (s *GenerationSession) completePlanningEvidence(evidence *provenance.PlanningEvidence, outcome string, response *string) (provenance.PlanningResponseIdentity, error) {
	s.planningEvidenceMu.Lock()
	defer s.planningEvidenceMu.Unlock()
	return evidence.Complete(outcome, response)
}

func generationFailure(primary, evidence error) error {
	return generationError(combineGenerationErrors(primary, evidence))
}

func combineGenerationErrors(primary, secondary error) error {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return fmt.Errorf("%w; planning evidence fail-closed also failed: %w", primary, secondary)
}

func (s *GenerationSession) recordTurnFailure(turnNumber uint64, startedAt time.Time, phase string) {
	data := s.servingProvenance()
	data["turn_number"] = turnNumber
	data["turn_phase"] = phase
	data["agent_contract_version"] = AgentContractVersion
	if phase == "planning" {
		data["thread_id"] = s.ThreadID()
		data["turn_id"] = s.currentTurnID()
	}
	data["duration_seconds"] = nonNegativeSeconds(time.Since(startedAt))
	data["result"] = "failed"
	event := "codex_turn_failed"
	if phase == "planning" {
		event = "planning_failed"
	}
	s.record(event, data)
	s.publish(observability.ActivityTurnFailed, map[string]any{"turn_number": turnNumber, "turn_phase": phase, "status": "failed"}, s.ThreadID(), s.currentTurnID(), "")
}

func (s *GenerationSession) recordInterviewTurnFailure(turnNumber uint64, interviewTurnNumber int, startedAt time.Time) {
	data := s.servingProvenance()
	data["interview_turn_number"] = interviewTurnNumber
	data["duration_seconds"] = nonNegativeSeconds(time.Since(startedAt))
	data["result"] = "failed"
	s.record("exit_interview_turn_failed", data)
	s.publish(observability.ActivityTurnFailed, map[string]any{
		"turn_number": turnNumber,
		"turn_phase":  "interview",
		"status":      "failed",
	}, s.ThreadID(), s.currentTurnID(), "")
}

func (s *GenerationSession) finishInterviewTurn(turnID string, response *string, status string) error {
	s.mu.Lock()
	transcript := s.interviewTranscript
	s.mu.Unlock()
	if transcript == nil {
		return &GenerationWorkerError{Reason: "exit interview transcript is unavailable"}
	}
	return transcript.FinishTurn(turnID, response, status)
}

func (s *GenerationSession) recordExitInterviewEnded(result string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.interviewStarted {
		s.mu.Unlock()
		return
	}
	s.interviewStarted = false
	threadID := s.threadID
	s.mu.Unlock()
	s.emitExitInterviewEnded(result, threadID)
}

func (s *GenerationSession) emitExitInterviewEnded(result, threadID string) {
	data := s.servingProvenance()
	data["result"] = result
	s.record("exit_interview_ended", data)
	s.publishRole(observability.ActivityHarness, observability.ActivityExitInterviewEnded, map[string]any{"result": result}, threadID, "", "")
}

func (s *GenerationSession) nextTurnNumber() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnNumber + 1
}
func (s *GenerationSession) currentTurnID() string { s.mu.Lock(); defer s.mu.Unlock(); return s.turnID }

func (s *GenerationSession) generationPointer() *uint64 {
	s.mu.Lock()
	generation, ok := s.generation, s.generationSet
	s.mu.Unlock()
	if !ok {
		return nil
	}
	return &generation
}

func (s *GenerationSession) sessionGeneration() (uint64, bool) {
	s.mu.Lock()
	if s.generationSet {
		generation := s.generation
		s.mu.Unlock()
		return generation, true
	}
	s.mu.Unlock()
	return s.runtime.GenerationNumber()
}

func (s *GenerationSession) belongsToCurrentGeneration() bool {
	s.mu.Lock()
	expected, set := s.generation, s.generationSet
	s.mu.Unlock()
	actual, ok := s.runtime.GenerationNumber()
	return set && ok && actual == expected
}

func (s *GenerationSession) runtimeState() string {
	if runtime, ok := any(s.runtime).(GenerationStateRuntime); ok {
		if state := runtime.GenerationState(); state != "" {
			return state
		}
	}
	if s.runtime.GenerationRunning() {
		return "running"
	}
	return "awaiting_next_generation"
}

func (s *GenerationSession) publish(kind observability.ActivityKind, data map[string]any, threadID, turnID, itemID string) {
	s.publishRole(observability.ActivityImplementor, kind, data, threadID, turnID, itemID)
}
func (s *GenerationSession) publishRole(role observability.ActivityRole, kind observability.ActivityKind, data map[string]any, threadID, turnID, itemID string) {
	observability.PublishActivity(s.options.ActivityStream, s.generationPointer(), role, kind, data, threadID, turnID, itemID)
}
func (s *GenerationSession) publishWithItem(kind observability.ActivityKind, data map[string]any, threadID, turnID, itemID string) {
	s.publish(kind, data, threadID, turnID, itemID)
}

func generationError(err error) error {
	if err == nil {
		return nil
	}
	if worker, ok := err.(*GenerationWorkerError); ok {
		return worker
	}
	return &GenerationWorkerError{Reason: err.Error(), Err: err}
}

func boundedGenerationError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > codexapp.MaxErrorOutput {
		return text[:codexapp.MaxErrorOutput]
	}
	return text
}

func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	if done == nil {
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func nonNegativeSeconds(duration time.Duration) float64 { return math.Max(0, duration.Seconds()) }

func resultSummary(status, state string) string {
	if status == "completed" && state == "running" {
		return "Codex turn completed; generation is still running."
	}
	if status == "completed" && state == "awaiting_next_generation" {
		return "Codex turn completed; generation completed cooperatively."
	}
	return fmt.Sprintf("Codex turn %s; generation state is %s.", status, state)
}

func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
func mergeMaps(left, right map[string]any) map[string]any {
	result := cloneMap(left)
	for key, value := range right {
		result[key] = value
	}
	return result
}

var planningTools = map[string]bool{
	"list": true, "read": true, "build": true, "request_feature": true,
	"list_provided_assets": true, "read_provided_asset": true, "list_requests": true,
}

func advertisedGuestTools(advertised []string) (map[string]struct{}, error) {
	selected, _, err := advertisedGuestToolsInOrder(advertised)
	return selected, err
}

func advertisedGuestToolsInOrder(advertised []string) (map[string]struct{}, []string, error) {
	registry := guestToolRegistry()
	selected := make(map[string]struct{}, len(advertised))
	for _, name := range advertised {
		if name == "" {
			return nil, nil, errors.New("guest tool list contains an invalid name")
		}
		if _, exists := selected[name]; exists {
			return nil, nil, fmt.Errorf("guest tool list contains duplicate name: %s", name)
		}
		selected[name] = struct{}{}
	}
	recognized := make(map[string]struct{})
	order := make([]string, 0, len(advertised))
	for _, name := range advertised {
		if _, ok := registry[name]; ok {
			recognized[name] = struct{}{}
			order = append(order, name)
		}
	}
	return recognized, order, nil
}

func guestToolRegistry() map[string]map[string]any {
	return map[string]map[string]any{
		"list":                 dynamicFunction("list", "List paths in the persistent mutable CodexOS guest source, optionally by prefix.", map[string]any{"prefix": map[string]any{"type": "string"}}, nil),
		"read":                 dynamicFunction("read", "Read exact bytes from the persistent mutable CodexOS guest source.", map[string]any{"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0}, "length": map[string]any{"type": "integer", "minimum": 0}}, []string{"path", "offset", "length"}),
		"write":                dynamicFunction("write", "Overwrite or append exact bytes in the persistent mutable CodexOS guest source.", map[string]any{"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0}, "encoding": map[string]any{"type": "string", "enum": []string{"utf8", "base64"}, "default": "utf8"}, "data": map[string]any{"type": "string"}}, []string{"path", "offset", "data"}),
		"truncate":             dynamicFunction("truncate", "Resize a file in the persistent mutable CodexOS guest source.", map[string]any{"path": map[string]any{"type": "string"}, "size": map[string]any{"type": "integer", "minimum": 0}}, []string{"path", "size"}),
		"remove":               dynamicFunction("remove", "Remove a file from the persistent mutable CodexOS guest source.", map[string]any{"path": map[string]any{"type": "string"}}, []string{"path"}),
		"build":                dynamicFunction("build", buildToolDescription, map[string]any{}, nil),
		"finish_generation":    dynamicFunction("finish_generation", finishGenerationToolDescription, map[string]any{"handoff": map[string]any{"type": "string"}}, []string{"handoff"}),
		"request_feature":      dynamicFunction("request_feature", requestFeatureToolDescription, map[string]any{"title": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, []string{"title", "description"}),
		"list_provided_assets": dynamicFunction("list_provided_assets", listProvidedAssetsToolDescription, map[string]any{}, nil),
		"read_provided_asset":  dynamicFunction("read_provided_asset", readProvidedAssetToolDescription, map[string]any{"id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0}, "length": map[string]any{"type": "integer", "minimum": 0}}, []string{"id", "offset", "length"}),
	}
}

func dynamicToolNamespace(selected map[string]struct{}) map[string]any {
	_, order, _ := advertisedGuestToolsInOrder([]string{"list", "read", "write", "truncate", "remove", "build", "finish_generation", "request_feature", "list_provided_assets", "read_provided_asset"})
	return dynamicToolNamespaceInOrder(selected, order)
}

func dynamicToolNamespaceInOrder(selected map[string]struct{}, order []string) map[string]any {
	registry := guestToolRegistry()
	tools := make([]map[string]any, 0, len(selected)+1)
	for _, name := range order {
		if _, ok := selected[name]; ok {
			tools = append(tools, registry[name])
		}
	}
	tools = append(tools, dynamicFunction("list_requests", listRequestsToolDescription, map[string]any{}, nil))
	return map[string]any{"type": "namespace", "name": "codexos", "description": "Develop the running CodexOS guest through its trusted tools.", "tools": tools}
}

func reviewDynamicFunction() map[string]any {
	return dynamicFunction("review", reviewToolDescription, map[string]any{
		"request": map[string]any{"type": "string"},
		"focus":   map[string]any{"type": "string", "enum": []string{"general", "correctness", "design", "security", "performance"}, "default": "general"},
	}, nil)
}

func validateGenerationToolCall(values map[string]any, threadID, turnID string) error {
	callID, ok := values["callId"].(string)
	if !ok || callID == "" {
		return errors.New("dynamic tool call ID must be a non-empty string")
	}
	if values["threadId"] != threadID {
		return errors.New("dynamic tool request has the wrong thread ID")
	}
	if values["turnId"] != turnID {
		return errors.New("dynamic tool request has the wrong turn ID")
	}
	return nil
}

func generationToolArguments(value any) (map[string]any, error) {
	arguments, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("dynamic tool arguments are not an object")
	}
	return arguments, nil
}
func checkGenerationFields(arguments map[string]any, required, optional map[string]struct{}) error {
	missing := make(map[string]struct{})
	for name := range required {
		if _, ok := arguments[name]; !ok {
			missing[name] = struct{}{}
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("missing argument: %s", firstGenerationField(missing))
	}
	unexpected := ""
	for name := range arguments {
		if _, ok := required[name]; ok {
			continue
		}
		if _, ok := optional[name]; ok {
			continue
		}
		if unexpected == "" || name < unexpected {
			unexpected = name
		}
	}
	if unexpected != "" {
		return fmt.Errorf("unexpected argument: %s", unexpected)
	}
	return nil
}
func firstGenerationField(fields map[string]struct{}) string {
	first := ""
	for name := range fields {
		if first == "" || name < first {
			first = name
		}
	}
	return first
}
func generationUTF8(value any, name string) ([]byte, error) {
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string", name)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("%s is not valid UTF-8", name)
	}
	return []byte(text), nil
}
func generationUnsignedDecimal(value any, name string) ([]byte, error) {
	var text string
	switch number := value.(type) {
	case json.Number:
		text = number.String()
	case uint64:
		text = strconv.FormatUint(number, 10)
	case uint:
		text = strconv.FormatUint(uint64(number), 10)
	case uint32:
		text = strconv.FormatUint(uint64(number), 10)
	case int:
		if number < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer", name)
		}
		text = strconv.Itoa(number)
	case int64:
		if number < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer", name)
		}
		text = strconv.FormatInt(number, 10)
	case int32:
		if number < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer", name)
		}
		text = strconv.FormatInt(int64(number), 10)
	default:
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	if text == "-0" {
		text = "0"
	}
	if text == "" || strings.HasPrefix(text, "-") {
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	if _, err := strconv.ParseUint(text, 10, 64); err != nil {
		return nil, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return []byte(text), nil
}
func generationData(arguments map[string]any) ([]byte, error) {
	encoding := "utf8"
	if value, ok := arguments["encoding"]; ok {
		var valid bool
		encoding, valid = value.(string)
		if !valid {
			return nil, errors.New("encoding must be 'utf8' or 'base64'")
		}
	}
	value, ok := arguments["data"].(string)
	if !ok {
		return nil, errors.New("data must be a string")
	}
	if encoding == "utf8" {
		return generationUTF8(value, "data")
	}
	if encoding != "base64" || strings.ContainsAny(value, "\r\n") {
		return nil, errors.New("encoding must be 'utf8' or 'base64'")
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func toolMetadata(tool string, arguments map[string]any) map[string]any {
	metadata := map[string]any{"input_bytes": 0}
	encoded := make([][]byte, 0)
	if path, ok := arguments["path"].(string); ok {
		metadata["path"] = path
	}
	for _, name := range []string{"offset", "length", "size"} {
		if value, ok := nonNegativeJSONInteger(arguments[name]); ok {
			metadata[name] = value
		}
	}
	for _, name := range []string{"prefix", "path", "id", "handoff", "title", "description"} {
		if value, ok := arguments[name].(string); ok && utf8.ValidString(value) {
			encoded = append(encoded, []byte(value))
		}
	}
	for _, name := range []string{"offset", "length", "size"} {
		if value, ok := nonNegativeJSONInteger(arguments[name]); ok {
			encoded = append(encoded, []byte(strconv.FormatUint(value, 10)))
		}
	}
	if tool == "write" {
		value, _ := arguments["data"].(string)
		encoding, _ := arguments["encoding"].(string)
		if encoding == "" {
			encoding = "utf8"
		}
		if encoding == "utf8" && utf8.ValidString(value) {
			encoded = append(encoded, []byte(value))
		}
		if encoding == "base64" && !strings.ContainsAny(value, "\r\n") {
			if data, err := base64.StdEncoding.DecodeString(value); err == nil {
				encoded = append(encoded, data)
			}
		}
	}
	total := 0
	for _, value := range encoded {
		total += len(value)
	}
	metadata["input_bytes"] = total
	return metadata
}
func nonNegativeJSONInteger(value any) (uint64, bool) {
	encoded, err := generationUnsignedDecimal(value, "integer")
	if err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseUint(string(encoded), 10, 64)
	return parsed, err == nil
}

func formatGenerationToolResult(result guest.ToolResult) string {
	output := ""
	encoding := "utf8"
	if utf8.Valid(result.Output) {
		output = string(result.Output)
	} else {
		output = base64.StdEncoding.EncodeToString(result.Output)
		encoding = "base64"
	}
	value := struct {
		Encoding string `json:"encoding"`
		Output   string `json:"output"`
		Status   uint32 `json:"status"`
	}{encoding, output, result.Status}
	var outputBuffer bytes.Buffer
	encoder := json.NewEncoder(&outputBuffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(value)
	if err != nil {
		return `{"encoding":"utf8","output":"","status":0}`
	}
	encoded := bytes.TrimSuffix(outputBuffer.Bytes(), []byte{'\n'})
	encoded = featureRequestJSONSeparators(encoded)
	return string(encoded)
}

func featureRequestsJSON(requests []store.FeatureRequest) ([]byte, error) {
	requests = append([]store.FeatureRequest(nil), requests...)
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	items := make([]struct {
		Description string `json:"description"`
		Generation  uint64 `json:"generation"`
		ID          uint64 `json:"id"`
		Status      string `json:"status"`
		Title       string `json:"title"`
	}, 0, len(requests))
	for _, request := range requests {
		items = append(items, struct {
			Description string `json:"description"`
			Generation  uint64 `json:"generation"`
			ID          uint64 `json:"id"`
			Status      string `json:"status"`
			Title       string `json:"title"`
		}{request.Description, request.Generation, request.ID, request.Status, request.Title})
	}
	value := struct {
		Requests any `json:"requests"`
	}{items}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
	encoded = featureRequestJSONSeparators(encoded)
	if len(encoded) > maxListRequestsOutputBytes {
		return nil, errors.New("serialized feature request state exceeds 16 MiB")
	}
	return encoded, nil
}

func planningPrompt(runtime GenerationRuntime, objective *string) (string, error) {
	handoff, handoffPresent, rollback, profile, requests, err := currentPromptContextFor(runtime)
	if err != nil {
		return "", err
	}
	handoffText := "Previous generation handoff: none."
	if handoffPresent {
		handoffText = "Previous generation handoff:\n" + handoff
	}
	rollbackText := ""
	if rollback {
		rollbackText = "\n\nThis generation was started from an earlier archived CodexOS state selected by the human operator. Later lineage was abandoned."
	}
	objectiveText := ""
	if objective != nil {
		objectiveText = "\n\nCurrent trusted objective:\n" + *objective
	}
	approved := make([]store.FeatureRequest, 0)
	for _, request := range requests {
		if request.Status == store.FeatureApproved {
			approved = append(approved, request)
		}
	}
	approvedText := "Approved external feature requests for this run: none."
	if len(approved) > 0 {
		parts := make([]string, 0, len(approved))
		for _, request := range approved {
			parts = append(parts, fmt.Sprintf("#%d: %s\n%s", request.ID, request.Title, request.Description))
		}
		approvedText = "Approved external feature requests for this run:\n\n" + strings.Join(parts, "\n\n")
	}
	return implementorContract + "\n\n" + trustedToolsContract() + "\n\n" + providedAssetsContract + "\n\n" + trustedHardwareContext(profile) + "\n\n" + approvedText + "\n\n" + handoffText + rollbackText + objectiveText, nil
}

func currentPromptContextFor(runtime GenerationRuntime) (string, bool, bool, qemu.HardwareProfile, []store.FeatureRequest, error) {
	var handoff string
	var handoffPresent bool
	var rollback bool
	var profile qemu.HardwareProfile
	var requests []store.FeatureRequest
	if value, ok := any(runtime).(interface{ PreviousHandoff() (string, bool) }); ok {
		handoff, handoffPresent = value.PreviousHandoff()
		// The presence bit is part of the runtime boundary: an explicitly
		// empty handoff is distinct from no predecessor handoff.
	}
	if value, ok := any(runtime).(interface{ CurrentTransition() (string, bool) }); ok {
		transition, _ := value.CurrentTransition()
		rollback = transition == "rollback"
	}
	if value, ok := any(runtime).(interface{ HardwareProfile() qemu.HardwareProfile }); ok {
		profile = value.HardwareProfile()
	}
	if value, ok := any(runtime).(interface {
		FeatureRequests() ([]store.FeatureRequest, error)
	}); ok {
		var err error
		requests, err = value.FeatureRequests()
		if err != nil {
			return handoff, handoffPresent, rollback, profile, nil, err
		}
	} else if value, ok := any(runtime).(interface{ FeatureRequests() []store.FeatureRequest }); ok {
		requests = value.FeatureRequests()
	}
	return handoff, handoffPresent, rollback, profile, requests, nil
}

func trustedHardwareContext(profile qemu.HardwareProfile) string {
	writable := strings.Join(profile.WritableBlockDevices, ", ")
	if writable == "" {
		writable = "none"
	}
	context := fmt.Sprintf("Current trusted hardware:\nProfile: %s\nMachine: %s\nAccelerator: %s\nCPU: %s\nvCPUs: %d\nRAM: %d MiB\nGraphics: %s\nNetwork interfaces: %s\nWritable block devices: %s\nThe standard VGA device is guest-visible while the current display frontend is headless.", profile.Profile, profile.Machine, profile.Accelerator, profile.CPUModel, profile.VCPUs, profile.MemoryMiB, profile.Graphics, profile.Network, writable)
	if profile.Machine == "q35" {
		context += " Normal q35 platform facilities remain available, including PCI/PCIe, ACPI, RTC, interrupt-controller, timer, and chipset facilities."
	}
	return context
}

func featureRequestJSONSeparators(encoded []byte) []byte {
	return bytes.ReplaceAll(bytes.ReplaceAll(encoded, []byte(`\u2028`), []byte("\u2028")), []byte(`\u2029`), []byte("\u2029"))
}
