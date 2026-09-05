package sourcecapacity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingPersistenceAndLegacyDefault(t *testing.T) {
	run := t.TempDir()
	budget, err := Load(run)
	if err != nil || budget.Bytes() != Default {
		t.Fatalf("legacy: %v %v", budget, err)
	}
	if err := Save(run, Expanded); err != nil {
		t.Fatal(err)
	}
	budget, err = Load(run)
	if err != nil || budget.Bytes() != Expanded {
		t.Fatalf("reopen: %v %v", budget, err)
	}
	fresh, err := Load(t.TempDir())
	if err != nil || fresh.Bytes() != Default {
		t.Fatal("setting escaped its run")
	}
	if err := Save(run, 1); err == nil {
		t.Fatal("invalid setting accepted")
	}
	budget, err = Load(run)
	if err != nil || budget.Bytes() != Expanded {
		t.Fatal("failed save changed setting")
	}
	for _, value := range []string{`{"schema_version":1,"content_bytes":0}`, `{"schema_version":2,"content_bytes":65536}`, `{"schema_version":1,"content_bytes":65536,"extra":true}`} {
		if err := os.WriteFile(filepath.Join(run, Filename), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(run); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
