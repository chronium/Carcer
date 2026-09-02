package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxFeatureTitleBytes       = 256
	MaxFeatureDescriptionBytes = 16 * 1024

	FeaturePending  = "pending"
	FeatureApproved = "approved"
	FeatureDenied   = "denied"
)

var (
	errJSONStringType    = errors.New("JSON value is not a string")
	errJSONStringUnicode = errors.New("JSON string contains an invalid Unicode surrogate")
)

type FeatureRequest struct {
	ID          uint64
	Generation  uint64
	Title       string
	Description string
	Status      string
}

type FeatureRequestError struct {
	Reason string
	Err    error
	// malformedRecord distinguishes JSON/UTF-8 decoding failures from a
	// successfully decoded record whose authoritative fields are invalid.
	malformedRecord bool
}

func (e *FeatureRequestError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *FeatureRequestError) Unwrap() error { return e.Err }

type FeatureRequestStore struct {
	directory string
	requests  map[uint64]FeatureRequest
}

func NewFeatureRequestStore(runDirectory string) (*FeatureRequestStore, error) {
	absolute, err := filepath.Abs(runDirectory)
	if err != nil {
		return nil, &FeatureRequestError{Reason: "could not resolve feature-request run directory", Err: err}
	}
	// Path.resolve() in the Python reference pins an existing symlinked run
	// directory to its physical target at construction time.
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return nil, &FeatureRequestError{Reason: "could not resolve feature-request run directory", Err: resolveErr}
	}
	store := &FeatureRequestStore{directory: filepath.Join(absolute, "feature-requests")}
	if err := store.refresh(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FeatureRequestStore) Requests() ([]FeatureRequest, error) {
	if err := s.refresh(); err != nil {
		return nil, err
	}
	identifiers := make([]uint64, 0, len(s.requests))
	for identifier := range s.requests {
		identifiers = append(identifiers, identifier)
	}
	sort.Slice(identifiers, func(i, j int) bool { return identifiers[i] < identifiers[j] })
	requests := make([]FeatureRequest, 0, len(identifiers))
	for _, identifier := range identifiers {
		requests = append(requests, s.requests[identifier])
	}
	return requests, nil
}

func (s *FeatureRequestStore) Request(requestID uint64) (FeatureRequest, error) {
	if err := validateRequestID(requestID); err != nil {
		return FeatureRequest{}, err
	}
	if err := s.refresh(); err != nil {
		return FeatureRequest{}, err
	}
	return s.cachedRequest(requestID)
}

func (s *FeatureRequestStore) Create(generation uint64, title, description string) (FeatureRequest, error) {
	if err := validateFeatureText(title, description); err != nil {
		return FeatureRequest{}, err
	}
	if err := s.refresh(); err != nil {
		return FeatureRequest{}, err
	}
	var requestID uint64 = 1
	for identifier := range s.requests {
		if identifier >= requestID {
			if identifier == ^uint64(0) {
				return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request ID space is exhausted"}
			}
			requestID = identifier + 1
		}
	}
	request := FeatureRequest{
		ID: requestID, Generation: generation, Title: title,
		Description: description, Status: FeaturePending,
	}
	if err := s.write(request, false); err != nil {
		return FeatureRequest{}, err
	}
	s.requests[requestID] = request
	return request, nil
}

func (s *FeatureRequestStore) Import(requests []FeatureRequest) error {
	if err := s.refresh(); err != nil {
		return err
	}
	if len(s.requests) != 0 {
		return &FeatureRequestError{Reason: "feature-request store is not empty"}
	}
	if _, err := os.Lstat(s.directory); err == nil {
		return &FeatureRequestError{Reason: "feature-request store is not empty"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return &FeatureRequestError{Reason: "could not inspect feature-request store", Err: err}
	}

	imported := make(map[uint64]FeatureRequest, len(requests))
	for _, request := range requests {
		if err := validateRequest(request); err != nil {
			return err
		}
		if _, exists := imported[request.ID]; exists {
			return &FeatureRequestError{Reason: fmt.Sprintf("duplicate imported feature request #%d", request.ID)}
		}
		imported[request.ID] = request
	}
	identifiers := make([]uint64, 0, len(imported))
	for identifier := range imported {
		identifiers = append(identifiers, identifier)
	}
	sort.Slice(identifiers, func(i, j int) bool { return identifiers[i] < identifiers[j] })
	for _, identifier := range identifiers {
		request := imported[identifier]
		if err := s.write(request, false); err != nil {
			return err
		}
		s.requests[identifier] = request
	}
	return nil
}

func (s *FeatureRequestStore) Approve(requestID uint64) (FeatureRequest, error) {
	return s.setStatus(requestID, FeatureApproved)
}

func (s *FeatureRequestStore) Deny(requestID uint64) (FeatureRequest, error) {
	return s.setStatus(requestID, FeatureDenied)
}

func (s *FeatureRequestStore) setStatus(requestID uint64, status string) (FeatureRequest, error) {
	if err := s.refresh(); err != nil {
		return FeatureRequest{}, err
	}
	if err := validateRequestID(requestID); err != nil {
		return FeatureRequest{}, err
	}
	current, err := s.cachedRequest(requestID)
	if err != nil {
		return FeatureRequest{}, err
	}
	if current.Status != FeaturePending {
		return FeatureRequest{}, &FeatureRequestError{Reason: fmt.Sprintf("feature request #%d is already %s", requestID, current.Status)}
	}
	current.Status = status
	if err := s.write(current, true); err != nil {
		return FeatureRequest{}, err
	}
	s.requests[requestID] = current
	return current, nil
}

func (s *FeatureRequestStore) cachedRequest(requestID uint64) (FeatureRequest, error) {
	request, exists := s.requests[requestID]
	if !exists {
		return FeatureRequest{}, &FeatureRequestError{Reason: fmt.Sprintf("feature request #%d does not exist", requestID)}
	}
	return request, nil
}

func (s *FeatureRequestStore) refresh() error {
	requests, err := readFeatureRequests(s.directory)
	if err != nil {
		return err
	}
	s.requests = requests
	return nil
}

func readFeatureRequests(directory string) (map[uint64]FeatureRequest, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[uint64]FeatureRequest), nil
	}
	if err != nil {
		return nil, &FeatureRequestError{Reason: "could not inspect feature-request store", Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, &FeatureRequestError{Reason: fmt.Sprintf("feature-request store is not a directory: %s", directory)}
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, &FeatureRequestError{Reason: "could not read feature-request store", Err: err}
	}
	requests := make(map[uint64]FeatureRequest, len(entries))
	for _, entry := range entries {
		requestID, err := requestIDFromName(entry.Name())
		if err != nil {
			return nil, err
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, &FeatureRequestError{Reason: fmt.Sprintf("invalid feature-request record: %s", entry.Name()), Err: err}
		}
		contents, err := os.ReadFile(path)
		if err != nil || !utf8.Valid(contents) {
			return nil, &FeatureRequestError{Reason: fmt.Sprintf("malformed feature-request record: %s", entry.Name()), Err: err}
		}
		request, err := DecodeFeatureRequest(contents)
		if err != nil {
			var featureErr *FeatureRequestError
			if errors.As(err, &featureErr) && featureErr.malformedRecord {
				return nil, &FeatureRequestError{Reason: fmt.Sprintf("malformed feature-request record: %s", entry.Name()), Err: err}
			}
			return nil, err
		}
		if request.ID != requestID {
			return nil, &FeatureRequestError{Reason: fmt.Sprintf("conflicting feature-request record: %s", entry.Name())}
		}
		if _, exists := requests[request.ID]; exists {
			return nil, &FeatureRequestError{Reason: fmt.Sprintf("conflicting feature-request record: %s", entry.Name())}
		}
		requests[request.ID] = request
	}
	return requests, nil
}

func DecodeFeatureRequest(contents []byte) (FeatureRequest, error) {
	if !utf8.Valid(contents) {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request record is not valid UTF-8", malformedRecord: true}
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request record is not valid JSON", Err: err, malformedRecord: true}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request record is not valid JSON", Err: err, malformedRecord: true}
	}
	wanted := []string{"id", "generation", "title", "description", "status"}
	if len(fields) != len(wanted) {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request record has invalid fields"}
	}
	for _, name := range wanted {
		if _, exists := fields[name]; !exists {
			return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request record has invalid fields"}
		}
	}

	requestID, err := decodeJSONUint(fields["id"])
	if err != nil || requestID == 0 {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request ID must be a positive integer"}
	}
	generation, err := decodeJSONUint(fields["generation"])
	if err != nil {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request generation is invalid"}
	}
	title, err := decodeJSONString(fields["title"])
	if err != nil {
		if errors.Is(err, errJSONStringUnicode) {
			return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request text is not valid UTF-8"}
		}
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request text must be strings"}
	}
	description, err := decodeJSONString(fields["description"])
	if err != nil {
		if errors.Is(err, errJSONStringUnicode) {
			return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request text is not valid UTF-8"}
		}
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request text must be strings"}
	}
	if err := validateFeatureText(title, description); err != nil {
		return FeatureRequest{}, err
	}
	status, err := decodeJSONString(fields["status"])
	if err != nil || !validFeatureStatus(status) {
		return FeatureRequest{}, &FeatureRequestError{Reason: "feature-request status is invalid"}
	}
	return FeatureRequest{ID: requestID, Generation: generation, Title: title, Description: description, Status: status}, nil
}

func (s *FeatureRequestStore) write(request FeatureRequest, replace bool) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	if err := os.MkdirAll(s.directory, 0o777); err != nil {
		return persistFeatureError(request.ID, err)
	}
	info, err := os.Lstat(s.directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err != nil {
			return persistFeatureError(request.ID, err)
		}
		return &FeatureRequestError{Reason: fmt.Sprintf("feature-request store is not a directory: %s", s.directory)}
	}

	destination := filepath.Join(s.directory, requestName(request.ID))
	if !replace {
		if _, err := os.Stat(destination); err == nil {
			return &FeatureRequestError{Reason: fmt.Sprintf("feature request #%d already exists", request.ID)}
		} else if !errors.Is(err, os.ErrNotExist) {
			return persistFeatureError(request.ID, err)
		}
	}
	encoded, err := encodeFeatureRequest(request)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.directory, "."+filepath.Base(destination)+".")
	if err != nil {
		return persistFeatureError(request.ID, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if written, err := temporary.Write(encoded); err != nil || written != len(encoded) {
		temporary.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return persistFeatureError(request.ID, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return persistFeatureError(request.ID, err)
	}
	if err := temporary.Close(); err != nil {
		return persistFeatureError(request.ID, err)
	}
	if !replace {
		if _, err := os.Stat(destination); err == nil {
			return &FeatureRequestError{Reason: fmt.Sprintf("feature request #%d already exists", request.ID)}
		} else if !errors.Is(err, os.ErrNotExist) {
			return persistFeatureError(request.ID, err)
		}
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return persistFeatureError(request.ID, err)
	}
	return nil
}

func encodeFeatureRequest(request FeatureRequest) ([]byte, error) {
	value := struct {
		Description string `json:"description"`
		Generation  uint64 `json:"generation"`
		ID          uint64 `json:"id"`
		Status      string `json:"status"`
		Title       string `json:"title"`
	}{
		Description: request.Description,
		Generation:  request.Generation,
		ID:          request.ID,
		Status:      request.Status,
		Title:       request.Title,
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, &FeatureRequestError{Reason: "could not encode feature request", Err: err}
	}
	return unescapeJSONLineSeparators(output.Bytes()), nil
}

// encoding/json escapes these two valid UTF-8 code points even when HTML
// escaping is disabled. Python's ensure_ascii=False writes them literally.
func unescapeJSONLineSeparators(encoded []byte) []byte {
	output := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if index+6 <= len(encoded) && encoded[index] == '\\' &&
			(string(encoded[index:index+6]) == `\u2028` || string(encoded[index:index+6]) == `\u2029`) {
			backslashes := 1
			for previous := index - 1; previous >= 0 && encoded[previous] == '\\'; previous-- {
				backslashes++
			}
			if backslashes%2 == 1 {
				if encoded[index+5] == '8' {
					output = append(output, 0xe2, 0x80, 0xa8)
				} else {
					output = append(output, 0xe2, 0x80, 0xa9)
				}
				index += 6
				continue
			}
		}
		output = append(output, encoded[index])
		index++
	}
	return output
}

func requestName(requestID uint64) string {
	return fmt.Sprintf("request-%06d.json", requestID)
}

func requestIDFromName(name string) (uint64, error) {
	if !strings.HasPrefix(name, "request-") || !strings.HasSuffix(name, ".json") {
		return 0, &FeatureRequestError{Reason: fmt.Sprintf("invalid feature-request filename: %s", name)}
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(name, "request-"), ".json")
	if encoded == "" {
		return 0, &FeatureRequestError{Reason: fmt.Sprintf("invalid feature-request filename: %s", name)}
	}
	for _, character := range encoded {
		if character < '0' || character > '9' {
			return 0, &FeatureRequestError{Reason: fmt.Sprintf("invalid feature-request filename: %s", name)}
		}
	}
	requestID, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil || requestID == 0 || requestName(requestID) != name {
		return 0, &FeatureRequestError{Reason: fmt.Sprintf("invalid feature-request filename: %s", name)}
	}
	return requestID, nil
}

func validateRequest(request FeatureRequest) error {
	if err := validateRequestID(request.ID); err != nil {
		return err
	}
	if err := validateFeatureText(request.Title, request.Description); err != nil {
		return err
	}
	if !validFeatureStatus(request.Status) {
		return &FeatureRequestError{Reason: "imported feature-request status is invalid"}
	}
	return nil
}

func validateRequestID(requestID uint64) error {
	if requestID == 0 {
		return &FeatureRequestError{Reason: "feature-request ID must be a positive integer"}
	}
	return nil
}

func validateFeatureText(title, description string) error {
	if !utf8.ValidString(title) || !utf8.ValidString(description) {
		return &FeatureRequestError{Reason: "feature-request text is not valid UTF-8"}
	}
	if title == "" {
		return &FeatureRequestError{Reason: "feature-request title must not be empty"}
	}
	if len(title) > MaxFeatureTitleBytes {
		return &FeatureRequestError{Reason: "feature-request title exceeds 256 bytes"}
	}
	if len(description) > MaxFeatureDescriptionBytes {
		return &FeatureRequestError{Reason: "feature-request description exceeds 16 KiB"}
	}
	return nil
}

func validFeatureStatus(status string) bool {
	return status == FeaturePending || status == FeatureApproved || status == FeatureDenied
}

func decodeJSONUint(raw json.RawMessage) (uint64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("not an integer")
	}
	return strconv.ParseUint(string(number), 10, 64)
}

func decodeJSONString(raw json.RawMessage) (string, error) {
	if !validJSONStringSurrogates(raw) {
		return "", errJSONStringUnicode
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	value, ok := decoded.(string)
	if !ok {
		return "", errJSONStringType
	}
	if !utf8.ValidString(value) {
		return "", errJSONStringUnicode
	}
	return value, nil
}

func validJSONStringSurrogates(raw []byte) bool {
	for index := 1; index+1 < len(raw); index++ {
		if raw[index] != '\\' {
			continue
		}
		index++
		if index >= len(raw)-1 || raw[index] != 'u' {
			continue
		}
		if index+4 >= len(raw) {
			return false
		}
		first, err := strconv.ParseUint(string(raw[index+1:index+5]), 16, 16)
		if err != nil {
			return false
		}
		index += 4
		if first >= 0xd800 && first <= 0xdbff {
			if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
				return false
			}
			second, err := strconv.ParseUint(string(raw[index+3:index+7]), 16, 16)
			if err != nil || second < 0xdc00 || second > 0xdfff {
				return false
			}
			index += 6
		} else if first >= 0xdc00 && first <= 0xdfff {
			return false
		}
	}
	return true
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("unexpected trailing JSON value")
	}
	return err
}

func persistFeatureError(requestID uint64, err error) error {
	var featureErr *FeatureRequestError
	if errors.As(err, &featureErr) {
		return err
	}
	return &FeatureRequestError{Reason: fmt.Sprintf("could not persist feature request #%d", requestID), Err: err}
}
