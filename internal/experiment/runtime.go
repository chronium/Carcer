// Package experiment owns the durable generation gate and the small amount of
// run state that can be recovered without starting any guest or agent
// process.  Process orchestration is deliberately outside this first slice.
package experiment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"codexos/internal/sourcecapacity"
)

const (
	// AbortMarker is the exact marker stored by the Python harness in an
	// aborted generation archive.
	AbortMarker = "Generation aborted by operator."

	archiveMetadataName  = "metadata.json"
	abortedMarkerName    = "aborted.txt"
	hardwareManifestName = "hardware.json"
	handoffName          = "handoff.txt"
	sourceSnapshotName   = "source.snapshot"
	archiveBootName      = "boot"
	sourceName           = "source"
	successorName        = "successor"
	stdoutName           = "qemu.stdout"
	stderrName           = "qemu.stderr"

	latestSuccessManifestName = "latest-success.json"
	latestSuccessSnapshotName = "latest-success.snapshot"

	archiveMetadataLimit         int64 = 64 * 1024
	archiveHardwareLimit         int64 = 1024 * 1024
	archiveHandoffLimit          int64 = 16 * 1024
	archiveForensicManifestLimit int64 = 1024 * 1024
	archiveHarnessIdentityLimit  int64 = 256 * 1024
	abortBootImageLimit          int64 = 128 * 1024 * 1024
)

// RuntimeState is the externally visible state of a run.  A gate is a
// terminal state for the current generation until an operator explicitly
// chooses ContinueGeneration or ForkFromGeneration.
type RuntimeState string

const (
	RuntimeStateStopped                RuntimeState = "stopped"
	RuntimeStateRunning                RuntimeState = "running"
	RuntimeStatePaused                 RuntimeState = "paused"
	RuntimeStateAwaitingNextGeneration RuntimeState = "awaiting_next_generation"
)

// ArchivedGeneration is the validated, immutable description of one archive.
// ArchivePath is resolved when the run is opened.  ParentGeneration and
// Handoff are nil only where the archive format permits them (generation zero
// has no parent; aborted generations have no handoff).
type ArchivedGeneration struct {
	Generation       uint64
	ParentGeneration *uint64
	Transition       string
	Outcome          string
	ArchivePath      string
	Handoff          *string
	AbortReason      *string
	Hardware         qemu.HardwareManifest
	HarnessIdentity  *provenance.HarnessIdentity
	SourceCapacity   sourcecapacity.Budget
}

// PendingGenerationFinish identifies the exact immutable successor selected
// by a completed archive.  The byte slice is private to the run and is copied
// when returned to callers.
type PendingGenerationFinish struct {
	HandoffMessage string
	SourceSnapshot []byte
	KernelELF      string
	ISO            string
}

// GenerationRuntimeError reports invalid durable generation state or an
// invalid state transition.  Err, when present, is the underlying filesystem
// or codec error.
type GenerationRuntimeError struct {
	Reason string
	Err    error
}

func (e *GenerationRuntimeError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	if e.Reason == "" {
		return e.Err.Error()
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *GenerationRuntimeError) Unwrap() error { return e.Err }

// CodexOSRun is the recoverable state of one run. NewCodexOSRun constructs the
// process-free archive/gate model; NewLiveCodexOSRun attaches concrete QEMU,
// serial, build, and guest-tool ownership to the same durable contract.
type CodexOSRun struct {
	sourceCapacity sourcecapacity.Budget
	gateMu         sync.Mutex
	runDirectory   string
	state          RuntimeState
	live           *liveRun

	generationNumber        *uint64
	previousHandoff         *string
	currentOperatorFeedback *OperatorFeedback
	pendingFinish           *PendingGenerationFinish

	currentParent     *uint64
	currentTransition string
	currentBootImage  string
	currentHardware   qemu.HardwareManifest
	retainedFinish    *uint64
	transitioning     bool

	gateHarnessTransition *provenance.HarnessGateTransition
}

// NewCodexOSRun opens a stopped run object.  Creating the run directory is
// the only mutation performed by construction; existing generation archives
// are not touched or repaired.
func NewCodexOSRun(runDirectory string) (*CodexOSRun, error) {
	run, err := resolveRunDirectory(runDirectory, true)
	if err != nil {
		return nil, &GenerationRuntimeError{Reason: "could not resolve run directory", Err: err}
	}
	budget, err := sourcecapacity.Load(run)
	if err != nil {
		return nil, err
	}
	return &CodexOSRun{
		sourceCapacity: budget,
		runDirectory:   run,
		state:          RuntimeStateStopped,
	}, nil
}

// RunDirectory returns the resolved run directory.
func (r *CodexOSRun) RunDirectory() string {
	if r == nil {
		return ""
	}
	return r.runDirectory
}

// ActivePID reports the concrete QEMU child owned by a live run.
func (r *CodexOSRun) ActivePID() (int, bool) {
	generation := r.liveGeneration()
	if generation == nil || generation.controller == nil {
		return 0, false
	}
	return generation.controller.PID()
}

// State returns the current run state.
func (r *CodexOSRun) State() RuntimeState {
	if r == nil {
		return RuntimeStateStopped
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return r.state
}

// GenerationNumber returns the current generation when this run has opened or
// started one.  A stopped, never-opened run returns (0, false).
func (r *CodexOSRun) GenerationNumber() (uint64, bool) {
	if r == nil {
		return 0, false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.generationNumber == nil {
		return 0, false
	}
	return *r.generationNumber, true
}

// RunPresentationSnapshot is the small runtime view needed by interactive
// frontends.
type RunPresentationSnapshot struct {
	SourceCapacity         sourcecapacity.Budget
	RunDirectory           string
	State                  RuntimeState
	Generation             uint64
	HasGeneration          bool
	PendingFeatureRequests int
	HarnessTransition      *provenance.HarnessGateTransition
}

// PresentationSnapshot never enters live operation serialization, so a guest
// exchange cannot prevent the operator interface from repainting or accepting
// input.
func (r *CodexOSRun) PresentationSnapshot() RunPresentationSnapshot {
	if r == nil {
		return RunPresentationSnapshot{State: RuntimeStateStopped}
	}
	snapshot := RunPresentationSnapshot{RunDirectory: r.runDirectory}
	r.gateMu.Lock()
	snapshot.State = r.state
	snapshot.SourceCapacity = r.sourceCapacity
	if r.generationNumber != nil {
		snapshot.Generation = *r.generationNumber
		snapshot.HasGeneration = true
	}
	snapshot.HarnessTransition = cloneHarnessGateTransition(r.gateHarnessTransition)
	r.gateMu.Unlock()
	if r.live != nil {
		snapshot.PendingFeatureRequests = int(r.live.pendingFeatures.Load())
	}
	return snapshot
}

// PreviousHandoff returns the handoff selected for the current generation.
func (r *CodexOSRun) PreviousHandoff() (string, bool) {
	if r == nil {
		return "", false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.previousHandoff == nil {
		return "", false
	}
	return *r.previousHandoff, true
}

// CurrentTransition returns how the current generation entered the lineage.
func (r *CodexOSRun) CurrentTransition() (string, bool) {
	if r == nil {
		return "", false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.currentTransition == "" {
		return "", false
	}
	return r.currentTransition, true
}

// PendingGenerationFinish returns a private copy of the selected completed
// successor, if the run is waiting at a completed-generation gate.
func (r *CodexOSRun) PendingGenerationFinish() (*PendingGenerationFinish, bool) {
	if r == nil {
		return nil, false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.pendingFinish == nil {
		return nil, false
	}
	pending := *r.pendingFinish
	pending.SourceSnapshot = append([]byte(nil), pending.SourceSnapshot...)
	return &pending, true
}

// GenerationFinishFrozen reports the gate invariant needed to retain a
// completed generation's Codex thread for a read-only exit interview. The
// caller separately verifies that it still owns this generation number.
func (r *CodexOSRun) GenerationFinishFrozen() bool {
	if r == nil {
		return false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return !r.transitioning && r.state == RuntimeStateAwaitingNextGeneration && r.pendingFinish != nil
}

// RetainGenerationFinish atomically leases one frozen completed-generation
// gate. Continue and rollback are rejected until the owning session releases
// the lease.
func (r *CodexOSRun) RetainGenerationFinish(generation uint64) bool {
	if r == nil {
		return false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.transitioning || r.retainedFinish != nil || r.state != RuntimeStateAwaitingNextGeneration ||
		r.pendingFinish == nil || r.generationNumber == nil || *r.generationNumber != generation {
		return false
	}
	r.retainedFinish = cloneUint64Pointer(&generation)
	return true
}

func (r *CodexOSRun) GenerationFinishRetained(generation uint64) bool {
	if r == nil {
		return false
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return r.retainedFinish != nil && *r.retainedFinish == generation &&
		!r.transitioning && r.state == RuntimeStateAwaitingNextGeneration && r.pendingFinish != nil
}

func (r *CodexOSRun) ReleaseGenerationFinish(generation uint64) {
	if r == nil {
		return
	}
	r.gateMu.Lock()
	if r.retainedFinish != nil && *r.retainedFinish == generation {
		r.retainedFinish = nil
	}
	r.gateMu.Unlock()
}

// ArchivedGenerations loads every generation archive in this run.  Individual
// archives are validated, but ancestry is intentionally checked by the caller
// through ValidateArchivedHistory or ReopenAtGate.
func (r *CodexOSRun) ArchivedGenerations() ([]ArchivedGeneration, error) {
	if r == nil {
		return nil, &GenerationRuntimeError{Reason: "run is nil"}
	}
	return LoadArchivedGenerations(r.runDirectory)
}

// InspectGeneration loads one archive without changing any run state.
func (r *CodexOSRun) InspectGeneration(generation uint64) (ArchivedGeneration, error) {
	if r == nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "run is nil"}
	}
	return InspectGeneration(r.runDirectory, generation)
}

// ReopenAtGate validates the complete archive history and restores only the
// gate state.  It never starts QEMU, Codex, or any other process.
func (r *CodexOSRun) ReopenAtGate() (resultErr error) {
	if r == nil {
		return &GenerationRuntimeError{Reason: "run is nil"}
	}
	if r.live != nil {
		r.live.operationMu.Lock()
		defer r.live.operationMu.Unlock()
		if r.live.closed {
			return &GenerationRuntimeError{Reason: "CodexOS run is closed"}
		}
	}
	r.gateMu.Lock()
	defer func() {
		r.gateMu.Unlock()
		if resultErr == nil && r.live != nil {
			r.setObservedLiveState()
		}
	}()
	if r.state != RuntimeStateStopped {
		return &GenerationRuntimeError{Reason: "CodexOS run is not stopped"}
	}
	if r.generationNumber != nil {
		return &GenerationRuntimeError{Reason: "CodexOS run has already been opened"}
	}
	partial, err := partialGenerationState(r.runDirectory)
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not inspect partial generation state", Err: err}
	}
	if len(partial) > 0 {
		return &GenerationRuntimeError{Reason: "run contains partial generation state: " + strings.Join(partial, ", ")}
	}
	archives, err := LoadArchivedGenerations(r.runDirectory)
	if err != nil {
		return err
	}
	if len(archives) == 0 {
		return &GenerationRuntimeError{Reason: "run has no archived generation gate"}
	}
	if err := ValidateArchivedHistory(archives); err != nil {
		return err
	}
	feedbackRecords, err := loadOperatorFeedbackRecords(r.runDirectory)
	if err != nil {
		return err
	}
	if err := validateOperatorFeedbackRecords(feedbackRecords, archives); err != nil {
		return err
	}

	latest := archives[len(archives)-1]
	var harnessTransition *provenance.HarnessGateTransition
	if r.live != nil && r.live.options.HarnessIdentity != nil {
		if r.live.harnessStore == nil {
			return &GenerationRuntimeError{Reason: "harness identity store is unavailable"}
		}
		prepared, err := r.live.harnessStore.PrepareGateTransition(*r.live.options.HarnessIdentity, latest.Generation)
		if err != nil {
			return err
		}
		harnessTransition = &prepared
	}
	if r.live != nil {
		effectiveGeneration := latest.Generation
		if effectiveGeneration != ^uint64(0) {
			effectiveGeneration++
		}
		if err := r.configureLiveAssets(effectiveGeneration); err != nil {
			return err
		}
	}
	var pending *PendingGenerationFinish
	var previous *string
	if latest.Outcome == "completed" {
		if latest.Handoff == nil {
			return &GenerationRuntimeError{Reason: "completed generation handoff is unavailable"}
		}
		snapshotPath := filepath.Join(latest.ArchivePath, sourceSnapshotName)
		snapshot, readErr := readRegularLimited(snapshotPath, latest.SourceCapacity.SnapshotLimit())
		if readErr != nil {
			return generationArchiveError(latest.Generation, readErr)
		}
		pending = &PendingGenerationFinish{
			HandoffMessage: *latest.Handoff,
			SourceSnapshot: append([]byte(nil), snapshot...),
			KernelELF:      filepath.Join(latest.ArchivePath, successorName, "kernel.elf"),
			ISO:            filepath.Join(latest.ArchivePath, successorName, "codexos.iso"),
		}
		value := *latest.Handoff
		previous = &value
	}
	if harnessTransition != nil && harnessTransition.RequiresRecord {
		if err := r.live.harnessStore.RecordGateTransition(*harnessTransition); err != nil {
			return err
		}
		r.recordLive("harness_identity_transition_recorded", &latest.Generation, map[string]any{
			"after_generation":  latest.Generation,
			"previous_identity": harnessIdentityJSON(harnessTransition.Previous),
			"current_identity":  harnessTransition.Current.AsJSON(),
		})
	}

	r.generationNumber = cloneUint64Pointer(&latest.Generation)
	r.pendingFinish = pending
	r.previousHandoff = previous
	r.currentOperatorFeedback = nil
	r.gateHarnessTransition = nil
	if harnessTransition != nil && harnessTransition.RequiresRecord {
		r.gateHarnessTransition = cloneHarnessGateTransition(harnessTransition)
	}
	if r.live != nil {
		r.currentParent = nil
		r.currentTransition = ""
		r.currentBootImage = ""
		r.currentHardware = qemu.HardwareManifest{}
	} else {
		r.currentParent = cloneUint64Pointer(latest.ParentGeneration)
		r.currentTransition = latest.Transition
		r.currentBootImage = filepath.Join(latest.ArchivePath, archiveBootName, "codexos.iso")
		r.currentHardware = latest.Hardware
	}
	r.state = RuntimeStateAwaitingNextGeneration
	if r.live != nil {
		r.live.started = true
		r.recordLive("run_reopened_at_gate", &latest.Generation, map[string]any{
			"latest_outcome": latest.Outcome, "successor_selected": pending != nil,
		})
	}
	return nil
}

func harnessIdentityJSON(identity *provenance.HarnessIdentity) any {
	if identity == nil {
		return nil
	}
	return identity.AsJSON()
}

func cloneHarnessGateTransition(transition *provenance.HarnessGateTransition) *provenance.HarnessGateTransition {
	if transition == nil {
		return nil
	}
	clone := *transition
	clone.Previous = provenance.CloneHarnessIdentity(transition.Previous)
	clone.Current = *provenance.CloneHarnessIdentity(&transition.Current)
	return &clone
}

// ContinueGeneration explicitly starts the next generation from the selected
// successor.  This method only advances the durable decision model; process
// boot and candidate validation belong to a later runtime slice.
func (r *CodexOSRun) ContinueGeneration() error {
	if r == nil {
		return &GenerationRuntimeError{Reason: "run is nil"}
	}
	if r.live != nil {
		ctx, cancel := context.WithTimeout(context.Background(), r.live.options.ReadyTimeout+15*time.Second)
		defer cancel()
		return r.continueLiveGeneration(ctx)
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.retainedFinish != nil {
		return &GenerationRuntimeError{Reason: "completed generation is retained for an exit interview"}
	}
	if r.state != RuntimeStateAwaitingNextGeneration {
		return &GenerationRuntimeError{Reason: "CodexOS run is not awaiting a generation"}
	}
	if r.pendingFinish == nil || r.generationNumber == nil {
		return &GenerationRuntimeError{Reason: "CodexOS run has no selected successor"}
	}
	if *r.generationNumber == ^uint64(0) {
		return &GenerationRuntimeError{Reason: "generation number space is exhausted"}
	}
	if !isRegularWithoutSymlink(r.pendingFinish.ISO) {
		return &GenerationRuntimeError{Reason: "selected successor artifact is missing: " + r.pendingFinish.ISO}
	}

	if _, err := guest.ParseSourceSnapshotWithBudget(r.pendingFinish.SourceSnapshot, r.sourceCapacity); err != nil {
		return err
	}
	parent := *r.generationNumber
	next := parent + 1
	image := r.pendingFinish.ISO
	handoff := r.pendingFinish.HandoffMessage
	feedback, err := r.attachOperatorFeedback(next)
	if err != nil {
		return err
	}
	r.generationNumber = &next
	r.currentParent = &parent
	r.currentTransition = "successor"
	r.currentBootImage = image
	r.previousHandoff = &handoff
	r.currentOperatorFeedback = feedback
	r.pendingFinish = nil
	r.gateHarnessTransition = nil
	r.state = RuntimeStateRunning
	return nil
}

// ForkFromGeneration explicitly selects an earlier completed archive's
// successor as the next generation.  The selected archive and the complete
// current history remain untouched.
func (r *CodexOSRun) ForkFromGeneration(generation uint64) error {
	if r == nil {
		return &GenerationRuntimeError{Reason: "run is nil"}
	}
	if r.live != nil {
		ctx, cancel := context.WithTimeout(context.Background(), r.live.options.ReadyTimeout+15*time.Second)
		defer cancel()
		return r.forkLiveGeneration(ctx, generation)
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.retainedFinish != nil {
		return &GenerationRuntimeError{Reason: "completed generation is retained for an exit interview"}
	}
	if r.state != RuntimeStateAwaitingNextGeneration {
		return &GenerationRuntimeError{Reason: "CodexOS run is not awaiting a generation"}
	}
	if r.generationNumber == nil {
		return &GenerationRuntimeError{Reason: "CodexOS generation number is unavailable"}
	}
	if generation >= *r.generationNumber {
		return &GenerationRuntimeError{Reason: "fork parent must be an earlier generation"}
	}
	if *r.generationNumber == ^uint64(0) {
		return &GenerationRuntimeError{Reason: "generation number space is exhausted"}
	}

	archived, err := InspectGeneration(r.runDirectory, generation)
	if err != nil {
		return err
	}
	if archived.Outcome != "completed" {
		return &GenerationRuntimeError{Reason: "aborted generation cannot be a rollback parent"}
	}
	if archived.Handoff == nil {
		return &GenerationRuntimeError{Reason: "completed generation handoff is unavailable"}
	}
	if err := validateInheritedSource(archived, r.sourceCapacity); err != nil {
		return err
	}
	image := filepath.Join(archived.ArchivePath, successorName, "codexos.iso")
	if !isRegularWithoutSymlink(image) {
		return &GenerationRuntimeError{Reason: "generation archive artifact is missing: " + image}
	}

	parent := generation
	next := *r.generationNumber + 1
	handoff := *archived.Handoff
	feedback, err := r.attachOperatorFeedback(next)
	if err != nil {
		return err
	}
	r.generationNumber = &next
	r.currentParent = &parent
	r.currentTransition = "rollback"
	r.currentBootImage = image
	r.previousHandoff = &handoff
	r.currentOperatorFeedback = feedback
	r.pendingFinish = nil
	r.gateHarnessTransition = nil
	r.state = RuntimeStateRunning
	return nil
}

// AbortGeneration publishes the current process-free generation as aborted
// and returns to the gate.  The process-owned QEMU logs are not available in
// this slice, so the durable archive records empty logs; a later runtime owner
// can supply captured logs through WriteAbortedArchive before exposing this
// transition in production.
func (r *CodexOSRun) AbortGeneration(reason string) error {
	if r == nil {
		return &GenerationRuntimeError{Reason: "run is nil"}
	}
	if r.live != nil {
		return r.abortLiveGeneration(reason)
	}
	if err := ValidateAbortReason(reason); err != nil {
		return err
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.state != RuntimeStateRunning {
		return &GenerationRuntimeError{Reason: "CodexOS generation cannot be aborted"}
	}
	if r.generationNumber == nil || r.currentTransition == "" || r.currentHardware.SchemaVersion == 0 {
		return &GenerationRuntimeError{Reason: "CodexOS generation state is unavailable"}
	}
	boot, err := readRegularLimited(r.currentBootImage, abortBootImageLimit)
	if err != nil {
		return &GenerationRuntimeError{Reason: "current generation boot image is unavailable", Err: err}
	}
	archive := AbortedArchive{
		SourceCapacity:   r.sourceCapacity,
		Generation:       *r.generationNumber,
		ParentGeneration: cloneUint64Pointer(r.currentParent),
		Transition:       r.currentTransition,
		Hardware:         r.currentHardware,
		BootISO:          boot,
		AbortReason:      reason,
	}
	if _, err := WriteAbortedArchive(r.runDirectory, archive); err != nil {
		return err
	}
	r.pendingFinish = nil
	r.retainedFinish = nil
	r.previousHandoff = nil
	r.currentOperatorFeedback = nil
	r.currentBootImage = ""
	r.state = RuntimeStateAwaitingNextGeneration
	return nil
}

// Stop retires the in-memory run state.  It never removes or rewrites an
// archive.  A stopped run object cannot be reopened after it has represented a
// generation, matching the Python runtime's one-object lifecycle.
func (r *CodexOSRun) Stop() {
	if r == nil {
		return
	}
	if err := r.stopLive(); err == nil {
		r.clearStoppedState()
	}
}

func (r *CodexOSRun) clearStoppedState() {
	if r == nil {
		return
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	r.pendingFinish = nil
	r.retainedFinish = nil
	r.previousHandoff = nil
	r.currentOperatorFeedback = nil
	r.currentParent = nil
	r.currentTransition = ""
	r.currentBootImage = ""
	r.currentHardware = qemu.HardwareManifest{}
	r.transitioning = false
	r.state = RuntimeStateStopped
}

// LoadArchivedGenerations reads and validates all generation directories in a
// run.  It performs no writes.  Use ValidateArchivedHistory when a gate or
// lineage decision is required.
func LoadArchivedGenerations(runDirectory string) ([]ArchivedGeneration, error) {
	run, err := resolveRunDirectory(runDirectory, false)
	if err != nil {
		return nil, &GenerationRuntimeError{Reason: "could not resolve run directory", Err: err}
	}
	entries, err := os.ReadDir(run)
	if err != nil {
		return nil, &GenerationRuntimeError{Reason: "could not inspect generation archives", Err: err}
	}
	archives := make([]ArchivedGeneration, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "generation-") {
			continue
		}
		generation, ok := parseASCIIUnsigned(strings.TrimPrefix(name, "generation-"))
		if !ok || name != generationName(generation) {
			return nil, &GenerationRuntimeError{Reason: "invalid generation archive: " + name}
		}
		archive, readErr := readArchivedGeneration(run, generation)
		if readErr != nil {
			return nil, generationArchiveError(generation, readErr)
		}
		archives = append(archives, archive)
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].Generation < archives[j].Generation })
	return archives, nil
}

// InspectGeneration reads and validates one archive without changing run
// state.
func InspectGeneration(runDirectory string, generation uint64) (ArchivedGeneration, error) {
	run, err := resolveRunDirectory(runDirectory, false)
	if err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "could not resolve run directory", Err: err}
	}
	archive, err := readArchivedGeneration(run, generation)
	if err != nil {
		return ArchivedGeneration{}, generationArchiveError(generation, err)
	}
	return archive, nil
}

// ValidateArchivedHistory enforces contiguous numbering and the Python
// successor/rollback ancestry rules.  It accepts a slice returned by
// LoadArchivedGenerations and does not touch the filesystem.
func ValidateArchivedHistory(archives []ArchivedGeneration) error {
	byGeneration := make(map[uint64]ArchivedGeneration, len(archives))
	for _, archive := range archives {
		if _, exists := byGeneration[archive.Generation]; exists {
			return &GenerationRuntimeError{Reason: "generation archive history is not contiguous"}
		}
		byGeneration[archive.Generation] = archive
	}
	for index, archive := range archives {
		if archive.Generation != uint64(index) {
			return &GenerationRuntimeError{Reason: "generation archive history is not contiguous"}
		}
	}
	for _, archive := range archives[1:] {
		if archive.ParentGeneration == nil {
			return &GenerationRuntimeError{Reason: fmt.Sprintf("generation %d has no completed parent", archive.Generation)}
		}
		parent, exists := byGeneration[*archive.ParentGeneration]
		if !exists || parent.Outcome != "completed" {
			return &GenerationRuntimeError{Reason: fmt.Sprintf("generation %d has no completed parent", archive.Generation)}
		}
		if archive.Transition == "successor" && *archive.ParentGeneration != archive.Generation-1 {
			return &GenerationRuntimeError{Reason: fmt.Sprintf("generation %d has invalid successor ancestry", archive.Generation)}
		}
		if archive.Transition == "rollback" && *archive.ParentGeneration == archive.Generation-1 {
			return &GenerationRuntimeError{Reason: fmt.Sprintf("generation %d has invalid rollback ancestry", archive.Generation)}
		}
	}
	return nil
}

func readArchivedGeneration(run string, generation uint64) (ArchivedGeneration, error) {
	archive := filepath.Join(run, generationName(generation))
	if !isDirectoryWithoutSymlink(archive) {
		return ArchivedGeneration{}, fmt.Errorf("generation archive is missing: %s", archive)
	}

	budget, err := sourcecapacity.Load(archive)
	if err != nil {
		return ArchivedGeneration{}, err
	}
	metadataBytes, err := readArchiveArtifact(filepath.Join(archive, archiveMetadataName), archiveMetadataLimit)
	if err != nil {
		return ArchivedGeneration{}, err
	}
	metadata, err := parseGenerationMetadata(metadataBytes, generation)
	if err != nil {
		return ArchivedGeneration{}, err
	}

	hardwareBytes, err := readArchiveArtifact(filepath.Join(archive, hardwareManifestName), archiveHardwareLimit)
	if err != nil {
		return ArchivedGeneration{}, err
	}
	hardware, err := qemu.ParseHardwareManifest(hardwareBytes)
	if err != nil {
		return ArchivedGeneration{}, errors.New("generation hardware manifest is malformed")
	}

	boot := filepath.Join(archive, archiveBootName)
	if !isDirectoryWithoutSymlink(boot) {
		return ArchivedGeneration{}, fmt.Errorf("generation archive artifact is missing: %s", boot)
	}
	if err := rejectSymlinkTree(boot); err != nil {
		return ArchivedGeneration{}, err
	}
	for _, required := range []string{
		filepath.Join(boot, "codexos.iso"),
		filepath.Join(archive, stdoutName),
		filepath.Join(archive, stderrName),
	} {
		if !isRegularWithoutSymlink(required) {
			return ArchivedGeneration{}, fmt.Errorf("generation archive artifact is missing: %s", required)
		}
	}
	var harnessIdentity *provenance.HarnessIdentity
	harnessPath := filepath.Join(archive, provenance.GenerationHarnessFilename)
	if _, statErr := os.Lstat(harnessPath); statErr == nil {
		harnessBytes, readErr := readArchiveArtifact(harnessPath, archiveHarnessIdentityLimit)
		if readErr != nil {
			return ArchivedGeneration{}, readErr
		}
		identity, parseErr := provenance.ParseHarnessIdentity(harnessBytes)
		if parseErr != nil {
			return ArchivedGeneration{}, errors.New("generation harness identity is malformed")
		}
		harnessIdentity = &identity
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ArchivedGeneration{}, statErr
	}

	result := ArchivedGeneration{
		SourceCapacity:   budget,
		Generation:       generation,
		ParentGeneration: cloneUint64Pointer(metadata.parent),
		Transition:       metadata.transition,
		Outcome:          metadata.outcome,
		ArchivePath:      archive,
		Hardware:         hardware,
		HarnessIdentity:  provenance.CloneHarnessIdentity(harnessIdentity),
	}
	if metadata.outcome == "completed" {
		for _, directory := range []string{
			filepath.Join(archive, sourceName),
			filepath.Join(archive, successorName),
		} {
			if !isDirectoryWithoutSymlink(directory) {
				return ArchivedGeneration{}, fmt.Errorf("generation archive artifact is missing: %s", directory)
			}
		}
		for _, directory := range []string{
			filepath.Join(archive, sourceName),
			filepath.Join(archive, successorName),
		} {
			if err := rejectSymlinkTree(directory); err != nil {
				return ArchivedGeneration{}, err
			}
		}
		for _, required := range []string{
			filepath.Join(archive, handoffName),
			filepath.Join(archive, sourceSnapshotName),
			filepath.Join(archive, successorName, "kernel.elf"),
			filepath.Join(archive, successorName, "codexos.iso"),
		} {
			if !isRegularWithoutSymlink(required) {
				return ArchivedGeneration{}, fmt.Errorf("generation archive artifact is missing: %s", required)
			}
		}
		handoffBytes, readErr := readArchiveArtifact(filepath.Join(archive, handoffName), archiveHandoffLimit)
		if readErr != nil || !utf8.Valid(handoffBytes) {
			return ArchivedGeneration{}, errors.New("generation handoff is not valid UTF-8")
		}
		snapshot, readErr := readArchiveArtifact(filepath.Join(archive, sourceSnapshotName), budget.SnapshotLimit())
		if readErr != nil {
			return ArchivedGeneration{}, readErr
		}
		if _, decodeErr := guest.DecodeSourceSnapshotWithBudget(snapshot, budget); decodeErr != nil {
			return ArchivedGeneration{}, decodeErr
		}
		handoff := string(handoffBytes)
		result.Handoff = &handoff
		names := []string{
			archiveBootName, archiveMetadataName, hardwareManifestName, handoffName,
			sourceSnapshotName, sourceName, successorName, stdoutName, stderrName,
		}
		if budget != 0 {
			names = append(names, sourcecapacity.Filename)
		}
		if harnessIdentity != nil {
			names = append(names, provenance.GenerationHarnessFilename)
		}
		if err := validateArchiveNames(archive, names); err != nil {
			return ArchivedGeneration{}, err
		}
	} else {
		abortedBytes, readErr := readArchiveArtifact(filepath.Join(archive, abortedMarkerName), archiveMetadataLimit)
		if readErr != nil {
			return ArchivedGeneration{}, readErr
		}
		if !bytes.Equal(abortedBytes, []byte(AbortMarker)) {
			return ArchivedGeneration{}, errors.New("generation abort marker is malformed")
		}
		reasonPath := filepath.Join(archive, abortReasonName)
		if _, statErr := os.Lstat(reasonPath); statErr == nil {
			reasonBytes, readErr := readArchiveArtifact(reasonPath, MaxAbortReasonBytes)
			if readErr != nil {
				return ArchivedGeneration{}, readErr
			}
			reason := string(reasonBytes)
			if err := ValidateAbortReason(reason); err != nil {
				return ArchivedGeneration{}, errors.New("generation abort reason is malformed")
			}
			result.AbortReason = &reason
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return ArchivedGeneration{}, statErr
		}
		names := []string{
			archiveBootName, archiveMetadataName, hardwareManifestName, abortedMarkerName,
			stdoutName, stderrName,
		}
		if result.AbortReason != nil {
			names = append(names, abortReasonName)
		}
		if budget != 0 {
			names = append(names, sourcecapacity.Filename)
		}
		if harnessIdentity != nil {
			names = append(names, provenance.GenerationHarnessFilename)
		}
		manifestPath := filepath.Join(archive, latestSuccessManifestName)
		snapshotPath := filepath.Join(archive, latestSuccessSnapshotName)
		manifestExists := pathExists(manifestPath)
		snapshotExists := pathExists(snapshotPath)
		if manifestExists || snapshotExists {
			if !isRegularWithoutSymlink(manifestPath) || !isRegularWithoutSymlink(snapshotPath) {
				return ArchivedGeneration{}, errors.New("aborted generation forensic evidence is incomplete")
			}
			if err := validateAbortedSuccessEvidence(manifestPath, snapshotPath, generation, budget); err != nil {
				return ArchivedGeneration{}, err
			}
			names = append(names, latestSuccessManifestName, latestSuccessSnapshotName)
		}
		if err := validateArchiveNames(archive, names); err != nil {
			return ArchivedGeneration{}, err
		}
	}
	return result, nil
}

type parsedGenerationMetadata struct {
	parent     *uint64
	transition string
	outcome    string
}

func parseGenerationMetadata(encoded []byte, expected uint64) (parsedGenerationMetadata, error) {
	var fields map[string]json.RawMessage
	if err := decodeJSON(encoded, &fields); err != nil || len(fields) != 4 {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	for _, key := range []string{"generation", "outcome", "parent_generation", "transition"} {
		if _, ok := fields[key]; !ok {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
	}
	generation, ok := parseJSONUnsigned(fields["generation"])
	if !ok || generation != expected {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata has the wrong generation")
	}
	var outcome string
	if err := json.Unmarshal(fields["outcome"], &outcome); err != nil || (outcome != "completed" && outcome != "aborted") {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	var transition string
	if err := json.Unmarshal(fields["transition"], &transition); err != nil {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	var parent *uint64
	if bytes.Equal(bytes.TrimSpace(fields["parent_generation"]), []byte("null")) {
		parent = nil
	} else {
		value, ok := parseJSONUnsigned(fields["parent_generation"])
		if !ok {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
		parent = &value
	}
	if generation == 0 {
		if parent != nil || transition != "initial" {
			return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
		}
	} else if parent == nil || *parent >= generation || (transition != "successor" && transition != "rollback") {
		return parsedGenerationMetadata{}, errors.New("generation archive metadata is malformed")
	}
	return parsedGenerationMetadata{parent: parent, transition: transition, outcome: outcome}, nil
}

func validateArchiveNames(directory string, expected []string) error {
	want := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		want[name] = struct{}{}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(want) {
		return fmt.Errorf("generation %s archive has invalid contents", filepath.Base(directory))
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			return fmt.Errorf("generation %s archive has invalid contents", filepath.Base(directory))
		}
	}
	return nil
}

func validateAbortedSuccessEvidence(manifestPath, snapshotPath string, generation uint64, budget sourcecapacity.Budget) error {
	manifestBytes, err := readArchiveArtifact(manifestPath, archiveForensicManifestLimit)
	if err != nil {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	manifest, err := decodeJSONWithUint64(manifestBytes)
	if err != nil {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	if value, ok := manifest["schema_version"].(uint64); !ok || value != 1 {
		return errors.New("aborted generation forensic manifest is malformed")
	}
	if value, ok := manifest["generation"].(uint64); !ok || value != generation {
		return errors.New("aborted generation forensic generation is incorrect")
	}
	if ready, ok := manifest["ready"].(bool); !ok || !ready {
		return errors.New("aborted generation forensic success is invalid")
	}
	if valid, ok := manifest["protocol_validated"].(bool); !ok || !valid {
		return errors.New("aborted generation forensic success is invalid")
	}
	attemptID, ok := manifest["build_attempt_id"].(string)
	if !ok || !strings.HasPrefix(attemptID, "build-") {
		return errors.New("aborted generation build attempt ID is invalid")
	}
	snapshot, err := readArchiveArtifact(snapshotPath, budget.SnapshotLimit())
	if err != nil {
		return errors.New("aborted generation source identity is invalid")
	}
	source, ok := manifest["source_snapshot"].(map[string]any)
	if !ok {
		return errors.New("aborted generation source identity is invalid")
	}
	if len(source) != 2 || source["sha256"] != sha256Hex(snapshot) || !sameJSONSize(source["size"], uint64(len(snapshot))) {
		return errors.New("aborted generation source identity is invalid")
	}
	if _, err := guest.DecodeSourceSnapshotWithBudget(snapshot, budget); err != nil {
		return errors.New("aborted generation source identity is invalid")
	}
	for _, name := range []string{"kernel", "iso"} {
		identity, ok := manifest[name].(map[string]any)
		if !ok || !isSHA256(identity["sha256"]) || !jsonNonNegativeSize(identity["size"]) {
			return fmt.Errorf("aborted generation %s identity is invalid", name)
		}
	}
	return nil
}

func decodeJSON(encoded []byte, output any) error {
	if !utf8.Valid(encoded) {
		return errors.New("invalid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func decodeJSONWithUint64(encoded []byte) (map[string]any, error) {
	if !utf8.Valid(encoded) {
		return nil, errors.New("invalid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	normalized, err := normalizeJSONNumbers(value)
	if err != nil {
		return nil, err
	}
	fields, ok := normalized.(map[string]any)
	if !ok {
		return nil, errors.New("JSON value is not an object")
	}
	return fields, nil
}

func normalizeJSONNumbers(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(string(value), 10, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case []any:
		for index, item := range value {
			normalized, err := normalizeJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		for key, item := range value {
			normalized, err := normalizeJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			value[key] = normalized
		}
		return value, nil
	default:
		return value, nil
	}
}

func parseJSONUnsigned(encoded []byte) (uint64, bool) {
	if bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
		return 0, false
	}
	var value uint64
	if err := json.Unmarshal(encoded, &value); err != nil {
		return 0, false
	}
	return value, true
}

func sameJSONSize(value any, expected uint64) bool {
	parsed, ok := value.(uint64)
	return ok && parsed == expected
}

func jsonNonNegativeSize(value any) bool {
	_, ok := value.(uint64)
	return ok
}

func isSHA256(value any) bool {
	text, ok := value.(string)
	if !ok || len(text) != 64 {
		return false
	}
	for _, character := range text {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func readRegularLimited(path string, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("archive read limit must not be negative")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("could not create generation archive file handle")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("generation archive path is not a regular file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("generation archive artifact exceeds its size limit")
	}
	return contents, nil
}

func readArchiveArtifact(path string, limit int64) ([]byte, error) {
	if !isRegularWithoutSymlink(path) {
		return nil, fmt.Errorf("generation archive artifact is missing: %s", path)
	}
	return readRegularLimited(path, limit)
}

func isRegularWithoutSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func isDirectoryWithoutSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func rejectSymlinkTree(root string) error {
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("generation archive artifact contains a symlink: %s", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func partialGenerationState(run string) ([]string, error) {
	entries, err := os.ReadDir(run)
	if err != nil {
		return nil, err
	}
	partial := make([]string, 0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".generation-") {
			partial = append(partial, entry.Name())
		}
	}
	sort.Strings(partial)
	return partial, nil
}

func resolveRunDirectory(path string, create bool) (string, error) {
	if path == "" {
		return "", errors.New("run directory must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if create {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("run directory is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !resolvedInfo.IsDir() {
		return "", errors.New("run directory is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func generationName(generation uint64) string {
	return fmt.Sprintf("generation-%04d", generation)
}

func parseASCIIUnsigned(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return parsed, err == nil
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func generationArchiveError(generation uint64, err error) error {
	if err == nil {
		return nil
	}
	return &GenerationRuntimeError{Reason: fmt.Sprintf("generation %d archive is invalid", generation), Err: err}
}
