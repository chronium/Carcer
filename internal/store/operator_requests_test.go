package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func operatorTestActor(role string) OperatorRequestActor {
	a := OperatorRequestActor{Role: role, Name: "test operator"}
	if role == "implementor" {
		generation := uint64(3)
		a.Name, a.Generation, a.ThreadID, a.TurnID = "test implementor", &generation, "thread-1", "turn-1"
	}
	return a
}

func TestOperatorRequestsDurableHistoryAndRetry(t *testing.T) {
	run := t.TempDir()
	s, err := NewOperatorRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.Create(operatorTestActor("operator"), "Interactive shell λ", "Allow a user to launch unrelated programs.")
	if err != nil {
		t.Fatal(err)
	}
	report := OperatorRequestRevision{Action: "disposition", Actor: operatorTestActor("implementor"), Disposition: "completed", Explanation: "Implemented generic launching.", Evidence: "Validated build 3; shell and unrelated workload progressed."}
	recorded, err := s.Append(request.ID, 1, report)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := s.Append(request.ID, 1, report)
	if err != nil || !reflect.DeepEqual(recorded, retried) {
		t.Fatalf("retry = %#v, %v", retried, err)
	}
	verified, err := s.Append(request.ID, 2, OperatorRequestRevision{Action: "verify", Actor: operatorTestActor("operator"), Explanation: "Observed the reported behavior.", ReportRevision: 2})
	if err != nil || verified.History[2].Actor.Role != "operator" {
		t.Fatalf("verification = %#v, %v", verified, err)
	}
	if _, err := s.Append(request.ID, 1, OperatorRequestRevision{Action: "withdraw", Actor: operatorTestActor("operator"), Explanation: "Out of scope."}); err == nil {
		t.Fatal("stale write accepted")
	}
	withdrawn, err := s.Append(request.ID, 3, OperatorRequestRevision{Action: "withdraw", Actor: operatorTestActor("operator"), Explanation: "No longer requested."})
	if err != nil || withdrawn.Active() {
		t.Fatalf("withdrawal = %#v, %v", withdrawn, err)
	}
	if _, err := s.Append(request.ID, 4, report); err == nil {
		t.Fatal("report after withdrawal accepted")
	}
	reopened, err := NewOperatorRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := reopened.Snapshot()
	if err != nil || ledger.Revision != 4 || !reflect.DeepEqual(ledger.Requests[0], withdrawn) {
		t.Fatalf("reopen = %#v, %v", ledger, err)
	}
	ledger.Requests[0].History[0].Actor.Name = "changed caller copy"
	second, err := reopened.Create(operatorTestActor("operator"), "Other capability", "A separate desired behavior.")
	if err != nil || second.ID != 2 {
		t.Fatalf("next request = %#v, %v", second, err)
	}
	again, _ := reopened.Snapshot()
	if again.Requests[0].History[0].Actor.Name != "test operator" {
		t.Fatal("caller mutation changed persisted history")
	}
	if _, err := os.Stat(filepath.Join(run, "feature-requests")); !os.IsNotExist(err) {
		t.Fatal("operator requests touched capability approvals")
	}
}

func TestOperatorRequestsRejectInvalidInputWithoutPublication(t *testing.T) {
	run := t.TempDir()
	s, err := NewOperatorRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", " \n\t", string([]byte{0xff}), strings.Repeat("λ", 129)} {
		if _, err := s.Create(operatorTestActor("operator"), value, "Description"); err == nil {
			t.Fatalf("accepted title %q", value)
		}
	}
	if _, err := os.Stat(filepath.Join(run, OperatorRequestsFilename)); !os.IsNotExist(err) {
		t.Fatal("invalid creation published state")
	}
	request, err := s.Create(operatorTestActor("operator"), "Desired capability", "Description")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(run, OperatorRequestsFilename))
	for _, entry := range []OperatorRequestRevision{
		{Action: "disposition", Actor: operatorTestActor("implementor"), Disposition: "completed", Explanation: "Done without evidence"},
		{Action: "disposition", Actor: operatorTestActor("operator"), Disposition: "pursuing", Explanation: "Wrong actor"},
		{Action: "verify", Actor: operatorTestActor("operator"), Explanation: "No report", ReportRevision: 1},
		{Action: "withdraw", Actor: operatorTestActor("operator"), Explanation: " "},
	} {
		if _, err := s.Append(request.ID, 1, entry); err == nil {
			t.Fatalf("accepted invalid entry %#v", entry)
		}
	}
	after, _ := os.ReadFile(filepath.Join(run, OperatorRequestsFilename))
	if !bytes.Equal(before, after) {
		t.Fatal("invalid mutation changed ledger")
	}
	for _, corrupt := range [][]byte{
		bytes.Replace(before, []byte(`"schema_version": 1`), []byte(`"schema_version": 2`), 1),
		bytes.Replace(before, []byte(`"Desired capability"`), []byte(`"\ud800"`), 1),
		append(append([]byte(nil), before...), []byte(`{}`)...),
		bytes.Replace(before, []byte(`"revision": 1`), []byte(`"revision": 8`), 1),
	} {
		if err := os.WriteFile(filepath.Join(run, OperatorRequestsFilename), corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewOperatorRequestStore(run); err == nil {
			t.Fatal("accepted corrupt ledger")
		}
	}
}

func TestOperatorRequestsConcurrentOwnersDoNotLoseHistory(t *testing.T) {
	run := t.TempDir()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			s, err := NewOperatorRequestStore(run)
			if err == nil {
				_, err = s.Create(operatorTestActor("operator"), "Capability", "Description")
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	s, err := NewOperatorRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := s.Snapshot()
	if err != nil || len(ledger.Requests) != 8 || ledger.Revision != 8 {
		t.Fatalf("concurrent ledger = %#v, %v", ledger, err)
	}
}

func TestOperatorRequestInheritancePreservesAttributionAndRejectsRewrites(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	s, err := NewOperatorRequestStore(source)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Create(operatorTestActor("operator"), "Capability", "Description")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append(r.ID, 1, OperatorRequestRevision{Action: "disposition", Actor: operatorTestActor("implementor"), Disposition: "deferred", Explanation: "Useful later."}); err != nil {
		t.Fatal(err)
	}
	parent, _ := s.Snapshot()
	if err = inheritOperatorRequests(source, destination, 3); err != nil {
		t.Fatal(err)
	}
	d, err := NewOperatorRequestStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := d.Snapshot()
	if err != nil || copy.RunID == parent.RunID || !reflect.DeepEqual(copy.Requests, parent.Requests) {
		t.Fatalf("inheritance = %#v, %v", copy, err)
	}
	if _, err = d.Create(operatorTestActor("operator"), "New capability", "Local request"); err != nil {
		t.Fatal(err)
	}
	copy, _ = d.Snapshot()
	if copy.Requests[1].History[0].Actor.RunID != copy.RunID {
		t.Fatal("new request lost destination attribution")
	}
	// The immutable source copy survives independently of the source directory.
	if err = os.Remove(filepath.Join(source, OperatorRequestsFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Snapshot(); err != nil {
		t.Fatal(err)
	}
	copy.Requests[0].History[0].Actor.Name = "rewritten author"
	data, _ := json.Marshal(copy)
	if err = os.WriteFile(filepath.Join(destination, OperatorRequestsFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Snapshot(); err == nil {
		t.Fatal("accepted rewritten inherited attribution")
	}
}

func TestCrossRunBootstrapCopiesOperatorRequestsSeparately(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := createCrossRunFixture(source); err != nil {
		t.Fatal(err)
	}
	s, err := NewOperatorRequestStore(source)
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.Create(operatorTestActor("operator"), "Capability", "Advisory OS behavior")
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	createCrossRunGitRepository(t, repository, "source/generation-0000")
	destination := filepath.Join(root, "destination")
	if _, err := InitializeCrossRunBootstrap(destination, filepath.Join(source, "generation-0000", "successor", "codexos.iso"), source, 0, repository, "source/generation-0000"); err != nil {
		t.Fatal(err)
	}
	d, err := NewOperatorRequestStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := d.Snapshot()
	if err != nil || len(ledger.Requests) != 1 || !reflect.DeepEqual(ledger.Requests[0], original) {
		t.Fatalf("copied operator requests = %#v, %v", ledger, err)
	}
	if _, err = LoadCrossRunBootstrap(destination); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(destination, OperatorRequestsFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadCrossRunBootstrap(destination); err == nil {
		t.Fatal("gate bootstrap accepted missing inherited OS requests")
	}
}
