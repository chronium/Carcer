package experiment

import (
	"testing"

	"codexos/internal/store"
)

func TestOperatorRequestReportsFollowSelectedLineage(t *testing.T) {
	directory := t.TempDir()
	for n := uint64(0); n < 2; n++ {
		var parent *uint64
		transition := "initial"
		if n > 0 {
			previous := n - 1
			parent = &previous
			transition = "successor"
		}
		_, err := WriteCompletedArchive(directory, CompletedArchive{Generation: n, ParentGeneration: parent, Transition: transition, Hardware: testHardware(t), BootISO: []byte("boot"), Handoff: "handoff", SourceSnapshot: testSnapshot(t, "source\n"), KernelELF: []byte("kernel"), SuccessorISO: []byte("iso")})
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewCodexOSRun(directory)
	if err != nil {
		t.Fatal(err)
	}
	request, err := r.CreateOperatorRequest("Example OS capability", "A disposable desired behavior")
	if err != nil {
		t.Fatal(err)
	}
	generation := uint64(1)
	actor := store.OperatorRequestActor{Role: "implementor", Name: "codex", Generation: &generation, ThreadID: "thread", TurnID: "turn"}
	request, err = r.operatorRequests.Append(request.ID, request.Revision(), store.OperatorRequestRevision{Action: "disposition", Actor: actor, Disposition: "completed", Explanation: "implemented", Evidence: "guest test transcript"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	context, err := r.OperatorRequests()
	if err != nil || context.Requests[0].Report == nil || context.Requests[0].Verification != nil {
		t.Fatalf("unverified report: %+v %v", context, err)
	}
	if _, err = r.VerifyOperatorRequest(1, 2, "observed behavior"); err != nil {
		t.Fatal(err)
	}
	// Reopening preserves verification; a selected rollback invalidates its claim.
	r, err = NewCodexOSRun(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.ReopenAtGate(); err != nil {
		t.Fatal(err)
	}
	context, err = r.OperatorRequests()
	if err != nil || context.Requests[0].Verification == nil {
		t.Fatalf("reopened: %+v %v", context, err)
	}
	if err = r.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
	context, err = r.OperatorRequests()
	if err != nil || context.Requests[0].Report != nil || !context.Requests[0].Active {
		t.Fatalf("rollback: %+v %v", context, err)
	}
	if _, err = r.VerifyOperatorRequest(1, 2, "old report"); err == nil {
		t.Fatal("verified excluded report")
	}
	generation = 2
	request, err = r.RecordOperatorRequest(1, 3, actor, "completed", "implemented again", "guest evidence")
	if err != nil {
		t.Fatal(err)
	}
	r.state = RuntimeStatePaused // Process-free model; no guest is started in this test.
	context, err = r.OperatorRequests()
	if err != nil || context.Requests[0].Report.Revision != request.Revision() {
		t.Fatalf("pause: %+v %v", context, err)
	}
	r.state = RuntimeStateRunning
	if err = r.AbortGeneration("disposable test"); err != nil {
		t.Fatal(err)
	}
	context, err = r.OperatorRequests()
	if err != nil || context.Requests[0].Report != nil {
		t.Fatalf("aborted claim: %+v %v", context, err)
	}
	historical, err := r.OperatorRequest(1)
	if err != nil || len(historical.History) != 4 {
		t.Fatalf("lost history: %+v %v", historical, err)
	}
	if _, err = r.RecordOperatorRequest(1, 4, actor, "completed", "late report", "evidence"); err == nil {
		t.Fatal("accepted report at aborted gate")
	}
	if _, err = r.WithdrawOperatorRequest(1, "no longer desired"); err != nil {
		t.Fatal(err)
	}
	if r.PresentationSnapshot().ActiveOperatorRequests != 0 {
		t.Fatal("withdrawn request remains active in TUI")
	}
	// No request status participates in the existing continuation decision.
	if err = r.ForkFromGeneration(0); err != nil {
		t.Fatal(err)
	}
}
