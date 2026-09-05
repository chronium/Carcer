package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFeatureRequestsPersistSparseUnicodeAndDecisions(t *testing.T) {
	run := t.TempDir()
	store, err := NewFeatureRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	inherited := []FeatureRequest{
		{ID: 2, Generation: 8, Title: "Pending λ", Description: "Exact <&> text", Status: FeaturePending},
		{ID: 7, Generation: 10, Title: "Denied", Description: "Exact denial", Status: FeatureDenied},
	}
	if err := store.Import(inherited); err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(0, "New run", "No collision")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 8 {
		t.Fatalf("created ID = %d, want 8", created.ID)
	}
	if err := store.Import(inherited); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second import error = %v", err)
	}

	reconstructed, err := NewFeatureRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := reconstructed.Requests()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]FeatureRequest(nil), inherited...), created)
	assertFeatureRequests(t, requests, want)
	approved, err := reconstructed.Approve(2, "")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != FeatureApproved {
		t.Fatalf("approved status = %q", approved.Status)
	}
	if _, err := reconstructed.Deny(7, ""); err == nil || !strings.Contains(err.Error(), "already denied") {
		t.Fatalf("deny decided request error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(run, "feature-requests"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	wantNames := []string{"request-000002.json", "request-000007.json", "request-000008.json"}
	if fmt.Sprint(names) != fmt.Sprint(wantNames) {
		t.Fatalf("record names = %v, want %v", names, wantNames)
	}
}

func TestFeatureRequestValidationAndMalformedRecords(t *testing.T) {
	store, err := NewFeatureRequestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, title, description, want string
	}{
		{name: "empty", title: "", description: "description", want: "must not be empty"},
		{name: "title bytes", title: strings.Repeat("é", 129), want: "256 bytes"},
		{name: "description bytes", title: "title", description: strings.Repeat("x", MaxFeatureDescriptionBytes+1), want: "16 KiB"},
		{name: "invalid UTF-8", title: string([]byte{255}), want: "UTF-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Create(0, test.title, test.description); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Create() error = %v, want %q", err, test.want)
			}
		})
	}
	request, err := store.Create(0, "../../outside.json", "../description is inert text")
	if err != nil || request.ID != 1 {
		t.Fatalf("path-like text create = %#v, %v", request, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.directory), "outside.json")); !os.IsNotExist(err) {
		t.Fatalf("request text escaped store: %v", err)
	}

	malformed := map[string]struct {
		contents []byte
		want     string
	}{
		"request-title.json":  {contents: []byte("{}"), want: "invalid feature-request filename"},
		"request-000001.json": {contents: []byte("{"), want: "malformed feature-request record"},
	}
	for name, test := range malformed {
		t.Run(name, func(t *testing.T) {
			run := t.TempDir()
			directory := filepath.Join(run, "feature-requests")
			if err := os.Mkdir(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, name), test.contents, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewFeatureRequestStore(run); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewFeatureRequestStore() error = %v, want %q", err, test.want)
			}
		})
	}

	for _, test := range []struct {
		contents []byte
		want     string
	}{
		{contents: []byte(`{"description":"d","generation":0,"id":2,"status":"pending","title":"t"}`), want: "conflicting feature-request record"},
		{contents: []byte(`{"description":"d","generation":0,"id":1,"status":"reopened","title":"t"}`), want: "status is invalid"},
		{contents: []byte(`{"description":"d","generation":0,"id":1,"status":"pending","title":"t","unexpected":true}`), want: "invalid fields"},
		{contents: []byte(`{"description":"d","generation":0,"id":true,"status":"pending","title":"t"}`), want: "positive integer"},
		{contents: []byte(`{"description":null,"generation":0,"id":1,"status":"pending","title":"t"}`), want: "text must be strings"},
		{contents: []byte(`{"description":"d","generation":0,"id":1,"status":"pending","title":null}`), want: "text must be strings"},
		{contents: []byte(`{"description":"d","generation":0,"id":1,"status":"pending","title":"\ud800"}`), want: "not valid UTF-8"},
	} {
		run := t.TempDir()
		directory := filepath.Join(run, "feature-requests")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "request-000001.json"), test.contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFeatureRequestStore(run); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("NewFeatureRequestStore() error = %v, want %q for %s", err, test.want, test.contents)
		}
	}
}

func TestFeatureRequestStorePinsExistingSymlinkedRun(t *testing.T) {
	target := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "run-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := NewFeatureRequestStore(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(0, "Pinned", "Original target"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "feature-requests", "request-000001.json")); err != nil {
		t.Fatalf("record was not written to resolved target: %v", err)
	}
}

func TestFeatureRequestReadersRefreshWithoutMutation(t *testing.T) {
	run := t.TempDir()
	reader, err := NewFeatureRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewFeatureRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	first, err := writer.Create(4, "Pending λ", "Exact pending text")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Request(first.ID); err != nil {
		t.Fatal(err)
	}
	approved, err := writer.Approve(first.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	observed, err := reader.Request(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed != approved {
		t.Fatalf("reader observed %#v, want %#v", observed, approved)
	}
	path := filepath.Join(run, "feature-requests", "request-000001.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Requests(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only refresh changed persisted bytes")
	}
}

func TestFeatureRequestWriteFailurePreservesCurrentRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode failure is Unix-specific")
	}
	run := t.TempDir()
	store, err := NewFeatureRequestStore(run)
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.Create(1, "Pending", "Exact text")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(run, "feature-requests", "request-000001.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(path)
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0o700)
	if _, err := store.Approve(request.ID, ""); err == nil || !strings.Contains(err.Error(), "could not persist") {
		t.Fatalf("Approve() error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed update replaced the current record")
	}
}

func FuzzDecodeFeatureRequest(f *testing.F) {
	valid, err := encodeFeatureRequest(FeatureRequest{
		ID: 3, Generation: 2, Title: "Title λ", Description: "binary-like \x00 text", Status: FeaturePending,
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(`{"id":true}`))
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, input []byte) {
		request, err := DecodeFeatureRequest(input)
		if err != nil {
			return
		}
		encoded, err := encodeFeatureRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := DecodeFeatureRequest(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if roundTrip != request {
			t.Fatalf("round trip = %#v, want %#v", roundTrip, request)
		}
	})
}

func assertFeatureRequests(t *testing.T, got, want []FeatureRequest) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d requests, want %d", len(got), len(want))
	}
	for index := range got {
		if got[index] != want[index] {
			t.Fatalf("request %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestFeatureDecisionNoteValidationAndPersistence(t *testing.T) {
	for _, approve := range []bool{true, false} {
		run := t.TempDir()
		s, err := NewFeatureRequestStore(run)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Create(3, "Guest title", "Guest description")
		if err != nil {
			t.Fatal(err)
		}
		decide := s.Approve
		wantStatus := FeatureApproved
		if !approve {
			decide = s.Deny
			wantStatus = FeatureDenied
		}
		for _, note := range []string{strings.Repeat("é", 2049), "bad\xff"} {
			if _, err := decide(r.ID, note); err == nil {
				t.Fatal("invalid note accepted")
			}
			got, err := s.Request(r.ID)
			if err != nil || got != r {
				t.Fatalf("failed validation changed record: %+v, %v", got, err)
			}
		}
		note := strings.Repeat("é", 2048)
		decided, err := decide(r.ID, note)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := NewFeatureRequestStore(run)
		if err != nil {
			t.Fatal(err)
		}
		got, err := reopened.Request(r.ID)
		if err != nil || got != decided || got.DecisionNote != note || got.Status != wantStatus || got.Description != r.Description {
			t.Fatalf("reloaded decision: %+v, %v", got, err)
		}
		if _, err := decide(r.ID, "replacement"); err == nil {
			t.Fatal("finalized decision note replaced")
		}
		contents, err := os.ReadFile(filepath.Join(run, "feature-requests", "request-000001.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`null`, `3`, `"\ud800"`, `"` + strings.Repeat("a", 4097) + `"`} {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(contents, &fields); err != nil {
				t.Fatal(err)
			}
			fields["decision_note"] = json.RawMessage(bad)
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeFeatureRequest(encoded); err == nil {
				t.Fatalf("invalid persisted note accepted: %s", bad)
			}
		}
	}
	if _, err := DecodeFeatureRequest([]byte(`{"id":1,"generation":0,"title":"t","description":"d","status":"pending","decision_note":"premature"}`)); err == nil {
		t.Fatal("pending note accepted")
	}
}
