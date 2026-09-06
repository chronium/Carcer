package experiment

import (
	"errors"
	"fmt"
	"os/user"
	"path/filepath"

	"codexos/internal/store"
)

// OperatorRequests is a fresh, lineage-aware view. Operator input is run-wide;
// implementor claims apply only to the current generation and completed parents.
func (r *CodexOSRun) OperatorRequests() (store.OperatorRequestContext, error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	return r.operatorRequestsLocked()
}

func (r *CodexOSRun) operatorRequestsLocked() (store.OperatorRequestContext, error) {
	ledger, err := r.operatorRequests.Snapshot()
	if err != nil {
		return store.OperatorRequestContext{}, err
	}
	eligible := map[uint64]bool{}
	if len(ledger.Requests) > 0 && r.generationNumber != nil {
		parent := r.currentParent
		if r.state == RuntimeStateRunning || r.state == RuntimeStatePaused {
			eligible[*r.generationNumber] = true
		} else if r.state == RuntimeStateAwaitingNextGeneration {
			parent = r.generationNumber
		}
		for parent != nil {
			generation := *parent
			raw, err := readArchiveArtifact(filepath.Join(r.runDirectory, generationName(generation), archiveMetadataName), archiveMetadataLimit)
			if err != nil {
				return store.OperatorRequestContext{}, err
			}
			metadata, err := parseGenerationMetadata(raw, generation)
			if err != nil {
				return store.OperatorRequestContext{}, err
			}
			if metadata.outcome == "completed" {
				eligible[generation] = true
			}
			parent = metadata.parent
			if parent != nil && *parent >= generation {
				return store.OperatorRequestContext{}, errors.New("invalid operator request report ancestry")
			}
		}
	}
	context := store.OperatorRequestContext{RunID: ledger.RunID, LedgerRevision: ledger.Revision, Requests: []store.OperatorRequestView{}}
	for _, request := range ledger.Requests {
		view := store.OperatorRequestView{ID: request.ID, Title: request.Title, Description: request.Description, Revision: request.Revision(), Active: request.Active(), Author: request.History[0].Actor}
		for _, event := range request.History {
			if event.Action == "disposition" && event.Actor.RunID == ledger.RunID && event.Actor.Generation != nil && eligible[*event.Actor.Generation] {
				report := event
				view.Report = &report
				view.Verification = nil
			}
			if event.Action == "verify" && view.Report != nil && event.ReportRevision == view.Report.Revision && event.Actor.RunID == ledger.RunID {
				verification := event
				view.Verification = &verification
			}
		}
		context.Requests = append(context.Requests, view)
	}
	return context, nil
}

func (r *CodexOSRun) OperatorRequest(id uint64) (store.OperatorRequest, error) {
	ledger, err := r.operatorRequests.Snapshot()
	if err != nil {
		return store.OperatorRequest{}, err
	}
	if id == 0 || id > uint64(len(ledger.Requests)) {
		return store.OperatorRequest{}, errors.New("operator request does not exist")
	}
	return ledger.Requests[id-1], nil
}

func (r *CodexOSRun) operatorRequestActorLocked() (store.OperatorRequestActor, error) {
	identity, err := user.Current()
	if err != nil {
		return store.OperatorRequestActor{}, fmt.Errorf("identify OS request operator: %w", err)
	}
	return store.OperatorRequestActor{Role: "operator", Name: identity.Username, Generation: cloneUint64Pointer(r.generationNumber)}, nil
}

func (r *CodexOSRun) CreateOperatorRequest(title, description string) (store.OperatorRequest, error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	actor, err := r.operatorRequestActorLocked()
	if err != nil {
		return store.OperatorRequest{}, err
	}
	request, err := r.operatorRequests.Create(actor, title, description)
	if err == nil {
		r.activeOperatorRequests++
	}
	return request, err
}

func (r *CodexOSRun) WithdrawOperatorRequest(id uint64, reason string) (store.OperatorRequest, error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	actor, err := r.operatorRequestActorLocked()
	if err != nil {
		return store.OperatorRequest{}, err
	}
	request, err := r.OperatorRequest(id)
	if err != nil {
		return request, err
	}
	updated, err := r.operatorRequests.Append(id, request.Revision(), store.OperatorRequestRevision{Action: "withdraw", Actor: actor, Explanation: reason})
	if err == nil {
		r.activeOperatorRequests--
	}
	return updated, err
}

func (r *CodexOSRun) VerifyOperatorRequest(id, revision uint64, note string) (store.OperatorRequest, error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	actor, err := r.operatorRequestActorLocked()
	if err != nil {
		return store.OperatorRequest{}, err
	}
	context, err := r.operatorRequestsLocked()
	if err != nil {
		return store.OperatorRequest{}, err
	}
	for _, request := range context.Requests {
		if request.ID == id && request.Active && request.Report != nil && request.Report.Disposition == "completed" && request.Report.Revision == revision {
			return r.operatorRequests.Append(id, request.Revision, store.OperatorRequestRevision{Action: "verify", Actor: actor, ReportRevision: revision, Explanation: note})
		}
	}
	return store.OperatorRequest{}, errors.New("verification requires the current applicable completion report revision")
}

func (r *CodexOSRun) RecordOperatorRequest(id, revision uint64, actor store.OperatorRequestActor, disposition, explanation, evidence string) (store.OperatorRequest, error) {
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.transitioning || r.state != RuntimeStateRunning || r.generationNumber == nil || actor.Generation == nil || *actor.Generation != *r.generationNumber || actor.Role != "implementor" {
		return store.OperatorRequest{}, errors.New("operator request reports require the active implementor generation")
	}
	return r.operatorRequests.Append(id, revision, store.OperatorRequestRevision{Action: "disposition", Actor: actor, Disposition: disposition, Explanation: explanation, Evidence: evidence})
}
