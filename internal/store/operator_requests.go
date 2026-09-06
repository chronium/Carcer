package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	OperatorRequestsFilename          = "operator-requests.json"
	InheritedOperatorRequestsFilename = "cross-run-operator-requests.json"
	MaxOperatorRequests               = 128
	MaxOperatorRequestTitleBytes      = 256
	MaxOperatorRequestTextBytes       = 4096
	MaxOperatorRequestEvidenceBytes   = 8192
	maxOperatorLedgerBytes            = 16 * 1024 * 1024
)

// Operator requests describe desired guest OS behavior. They are never entries
// in the guest-to-operator capability approval ledger.
type OperatorRequestActor struct {
	Role       string  `json:"role"`
	Name       string  `json:"name"`
	RunID      string  `json:"run_id"`
	Generation *uint64 `json:"generation,omitempty"`
	ThreadID   string  `json:"thread_id,omitempty"`
	TurnID     string  `json:"turn_id,omitempty"`
}

type OperatorRequestRevision struct {
	Revision       uint64               `json:"revision"`
	Action         string               `json:"action"`
	Actor          OperatorRequestActor `json:"actor"`
	Timestamp      string               `json:"timestamp"`
	Disposition    string               `json:"disposition,omitempty"`
	Explanation    string               `json:"explanation,omitempty"`
	Evidence       string               `json:"evidence,omitempty"`
	ReportRevision uint64               `json:"report_revision,omitempty"`
}

type OperatorRequest struct {
	ID          uint64                    `json:"id"`
	Title       string                    `json:"title"`
	Description string                    `json:"description"`
	History     []OperatorRequestRevision `json:"history"`
}

func (r OperatorRequest) Revision() uint64 { return uint64(len(r.History)) }
func (r OperatorRequest) Active() bool {
	return len(r.History) != 0 && r.History[len(r.History)-1].Action != "withdraw"
}

type OperatorRequestInheritance struct {
	RunID      string `json:"run_id"`
	Generation uint64 `json:"generation"`
	SHA256     string `json:"sha256"`
}

type OperatorRequestLedger struct {
	SchemaVersion int                         `json:"schema_version"`
	RunID         string                      `json:"run_id"`
	Revision      uint64                      `json:"revision"`
	Inherited     *OperatorRequestInheritance `json:"inherited,omitempty"`
	Requests      []OperatorRequest           `json:"requests"`
}

// Context is the exact revision projection delivered at an implementor turn
// boundary. Completion reports are claims; Verification identifies a separate
// operator attestation of the exact report, never automatic acceptance.
type OperatorRequestContext struct {
	RunID          string                `json:"run_id"`
	LedgerRevision uint64                `json:"ledger_revision"`
	Requests       []OperatorRequestView `json:"requests"`
}

type OperatorRequestView struct {
	ID           uint64                   `json:"id"`
	Title        string                   `json:"title"`
	Description  string                   `json:"description"`
	Revision     uint64                   `json:"revision"`
	Active       bool                     `json:"active"`
	Author       OperatorRequestActor     `json:"author"`
	Report       *OperatorRequestRevision `json:"implementor_report,omitempty"`
	Verification *OperatorRequestRevision `json:"operator_verification,omitempty"`
}

// One concrete owner serializes operator commands and agent reports. The file
// lock also prevents lost updates when independently opened owners overlap.
type OperatorRequestStore struct {
	mu  sync.Mutex
	run string
}

func NewOperatorRequestStore(run string) (*OperatorRequestStore, error) {
	absolute, err := filepath.Abs(run)
	if err != nil {
		return nil, err
	}
	s := &OperatorRequestStore{run: absolute}
	_, err = s.Snapshot()
	return s, err
}

func (s *OperatorRequestStore) Snapshot() (OperatorRequestLedger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readOperatorRequests(s.run)
}

func ValidateOperatorRequestText(title, description string) error {
	if err := operatorRequestText(title, MaxOperatorRequestTitleBytes, "title"); err != nil {
		return err
	}
	return operatorRequestText(description, MaxOperatorRequestTextBytes, "description")
}

func operatorRequestText(text string, limit int, field string) error {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || len(text) > limit {
		return fmt.Errorf("operator request %s must be non-empty UTF-8 text of at most %d bytes", field, limit)
	}
	return nil
}

func (s *OperatorRequestStore) Create(actor OperatorRequestActor, title, description string) (OperatorRequest, error) {
	if err := ValidateOperatorRequestText(title, description); err != nil {
		return OperatorRequest{}, err
	}
	if actor.Role != "operator" {
		return OperatorRequest{}, errors.New("only an operator can create an OS request")
	}
	return s.change(func(ledger *OperatorRequestLedger) (OperatorRequest, error) {
		if len(ledger.Requests) >= MaxOperatorRequests {
			return OperatorRequest{}, errors.New("operator request limit reached")
		}
		actor.RunID = ledger.RunID
		request := OperatorRequest{ID: uint64(len(ledger.Requests)) + 1, Title: title, Description: description,
			History: []OperatorRequestRevision{{Revision: 1, Action: "create", Actor: actor, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}}}
		ledger.Requests = append(ledger.Requests, request)
		ledger.Revision++
		return request, nil
	})
}

// Append uses a request revision rather than a global ledger revision: an
// unrelated operator request cannot invalidate a report. A repeated last write
// with the same attribution and expected revision returns the original record.
func (s *OperatorRequestStore) Append(id, expected uint64, entry OperatorRequestRevision) (OperatorRequest, error) {
	return s.change(func(ledger *OperatorRequestLedger) (OperatorRequest, error) {
		if id == 0 || id > uint64(len(ledger.Requests)) {
			return OperatorRequest{}, errors.New("operator request does not exist")
		}
		request := &ledger.Requests[id-1]
		entry.Actor.RunID = ledger.RunID
		entry.Revision = expected + 1
		last := request.History[len(request.History)-1]
		entry.Timestamp = last.Timestamp
		if reflect.DeepEqual(entry, last) {
			return *request, nil
		}
		if expected != request.Revision() {
			return OperatorRequest{}, errors.New("operator request revision changed; use its latest presented revision")
		}
		if !request.Active() {
			return OperatorRequest{}, errors.New("operator request is withdrawn")
		}
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		request.History = append(request.History, entry)
		ledger.Revision++
		return *request, nil
	})
}

func (s *OperatorRequestStore) change(update func(*OperatorRequestLedger) (OperatorRequest, error)) (OperatorRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := os.OpenFile(filepath.Join(s.run, ".operator-requests.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return OperatorRequest{}, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return OperatorRequest{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ledger, err := readOperatorRequests(s.run)
	if err != nil {
		return OperatorRequest{}, err
	}
	if ledger.RunID == "" {
		ledger.RunID, err = newOperatorRunID()
		if err != nil {
			return OperatorRequest{}, err
		}
	}
	request, err := update(&ledger)
	if err != nil {
		return OperatorRequest{}, err
	}
	if err = writeOperatorRequests(s.run, ledger); err != nil {
		return OperatorRequest{}, err
	}
	return request, nil
}

func newOperatorRunID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func readOperatorRequests(run string) (OperatorRequestLedger, error) {
	data, err := readCrossRunLimited(filepath.Join(run, OperatorRequestsFilename), maxOperatorLedgerBytes)
	if errors.Is(err, os.ErrNotExist) {
		if _, inheritedErr := os.Lstat(filepath.Join(run, InheritedOperatorRequestsFilename)); !errors.Is(inheritedErr, os.ErrNotExist) {
			return OperatorRequestLedger{}, errors.New("inherited operator request ledger is missing")
		}
		return OperatorRequestLedger{SchemaVersion: 1, Requests: []OperatorRequest{}}, nil
	}
	if err != nil {
		return OperatorRequestLedger{}, fmt.Errorf("read operator requests: %w", err)
	}
	ledger, err := decodeOperatorRequests(data)
	if err != nil {
		return OperatorRequestLedger{}, err
	}
	if err = validateOperatorInheritance(run, ledger); err != nil {
		return OperatorRequestLedger{}, err
	}
	return ledger, nil
}

func decodeOperatorRequests(data []byte) (OperatorRequestLedger, error) {
	var ledger OperatorRequestLedger
	if !utf8.Valid(data) || !validJSONStringSurrogates(data) {
		return ledger, errors.New("operator request ledger contains invalid Unicode")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return ledger, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ledger, err
	}
	return ledger, validateOperatorRequests(ledger)
}

func validateOperatorRequests(ledger OperatorRequestLedger) error {
	if ledger.SchemaVersion != 1 || !operatorRunIDValid(ledger.RunID) || ledger.Requests == nil || len(ledger.Requests) > MaxOperatorRequests {
		return errors.New("malformed operator request ledger")
	}
	var revisions uint64
	for index, request := range ledger.Requests {
		if request.ID != uint64(index)+1 || len(request.History) == 0 || len(request.History) > 4096 {
			return errors.New("malformed operator request history")
		}
		if err := ValidateOperatorRequestText(request.Title, request.Description); err != nil {
			return err
		}
		withdrawn := false
		for i, entry := range request.History {
			if withdrawn || entry.Revision != uint64(i)+1 {
				return errors.New("operator request history is not contiguous or follows withdrawal")
			}
			if _, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err != nil {
				return err
			}
			a := entry.Actor
			if !operatorRunIDValid(a.RunID) || operatorRequestText(a.Name, 256, "author") != nil {
				return errors.New("invalid operator request attribution")
			}
			if a.Role == "implementor" {
				if a.Generation == nil || operatorRequestText(a.ThreadID, 256, "thread") != nil || operatorRequestText(a.TurnID, 256, "turn") != nil {
					return errors.New("implementor attribution requires generation, thread, and turn")
				}
			} else if a.Role != "operator" || a.ThreadID != "" || a.TurnID != "" {
				return errors.New("invalid operator request actor")
			}
			if i == 0 {
				if entry.Action != "create" || a.Role != "operator" || entry.Explanation != "" || entry.Disposition != "" || entry.Evidence != "" || entry.ReportRevision != 0 {
					return errors.New("invalid operator request creation")
				}
				continue
			}
			if err := operatorRequestText(entry.Explanation, MaxOperatorRequestTextBytes, "explanation"); err != nil {
				return err
			}
			if !utf8.ValidString(entry.Evidence) || len(entry.Evidence) > MaxOperatorRequestEvidenceBytes {
				return errors.New("operator request evidence exceeds its UTF-8 byte limit")
			}
			switch entry.Action {
			case "disposition":
				if a.Role != "implementor" || entry.ReportRevision != 0 {
					return errors.New("invalid disposition attribution")
				}
				switch entry.Disposition {
				case "pursuing", "deferred", "declined":
				case "completed":
					if operatorRequestText(entry.Evidence, MaxOperatorRequestEvidenceBytes, "completion evidence") != nil {
						return errors.New("reported completion requires evidence")
					}
				default:
					return errors.New("unsupported operator request disposition")
				}
			case "withdraw", "verify":
				if a.Role != "operator" || entry.Disposition != "" || entry.Evidence != "" {
					return errors.New("invalid operator request decision")
				}
				if entry.Action == "withdraw" {
					if entry.ReportRevision != 0 {
						return errors.New("withdrawal cannot verify a report")
					}
					withdrawn = true
				} else {
					if entry.ReportRevision == 0 || entry.ReportRevision >= entry.Revision {
						return errors.New("verification must reference an earlier report")
					}
					report := request.History[entry.ReportRevision-1]
					if report.Action != "disposition" || report.Disposition != "completed" || report.Actor.RunID != a.RunID {
						return errors.New("verification must reference a completion report in this run")
					}
				}
			default:
				return errors.New("unsupported operator request history action")
			}
		}
		revisions += uint64(len(request.History))
	}
	if ledger.Revision != revisions {
		return errors.New("operator request ledger revision does not match its history")
	}
	return nil
}

func operatorRunIDValid(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && value == strings.ToLower(value)
}

func writeOperatorRequests(run string, ledger OperatorRequestLedger) error {
	if err := validateOperatorRequests(ledger); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxOperatorLedgerBytes {
		return errors.New("operator request history exceeds 16 MiB")
	}
	file, err := os.CreateTemp(run, ".operator-requests-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(run, OperatorRequestsFilename)); err != nil {
		return err
	}
	return syncCrossRunDirectory(run, false)
}

func inheritOperatorRequests(source, destination string, generation uint64) error {
	ledger, err := readOperatorRequests(source)
	if err != nil || ledger.RunID == "" {
		return err
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	if err = writeCrossRunDurable(filepath.Join(destination, InheritedOperatorRequestsFilename), data); err != nil {
		return err
	}
	ledger.Inherited = &OperatorRequestInheritance{RunID: ledger.RunID, Generation: generation, SHA256: crossRunBytesIdentity(data).SHA256}
	ledger.RunID, err = newOperatorRunID()
	if err != nil {
		return err
	}
	return writeOperatorRequests(destination, ledger)
}

func validateOperatorInheritance(run string, ledger OperatorRequestLedger) error {
	data, err := readCrossRunLimited(filepath.Join(run, InheritedOperatorRequestsFilename), maxOperatorLedgerBytes)
	if ledger.Inherited == nil {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("unexpected operator request inheritance record")
		}
		for _, request := range ledger.Requests {
			for _, entry := range request.History {
				if entry.Actor.RunID != ledger.RunID {
					return errors.New("operator request attribution has an unknown run")
				}
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read inherited operator requests: %w", err)
	}
	parent, err := decodeOperatorRequests(data)
	if err != nil {
		return err
	}
	if parent.RunID != ledger.Inherited.RunID || parent.RunID == ledger.RunID || crossRunBytesIdentity(data).SHA256 != ledger.Inherited.SHA256 || len(ledger.Requests) < len(parent.Requests) {
		return errors.New("operator request inheritance identity differs")
	}
	for i, request := range ledger.Requests {
		start := 0
		if i < len(parent.Requests) {
			original := parent.Requests[i]
			start = len(original.History)
			if request.Title != original.Title || request.Description != original.Description || len(request.History) < start || !reflect.DeepEqual(request.History[:start], original.History) {
				return errors.New("inherited operator request history was altered")
			}
		}
		for _, entry := range request.History[start:] {
			if entry.Actor.RunID != ledger.RunID {
				return errors.New("new request history has foreign attribution")
			}
		}
	}
	return nil
}
