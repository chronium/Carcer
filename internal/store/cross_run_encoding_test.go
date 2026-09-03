package store

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCrossRunTypedEncodersPreserveCanonicalWireBytes(t *testing.T) {
	requests := []FeatureRequest{{
		ID: 7, Generation: 3, Title: "Need <capacity>", Description: "Handle λ safely", Status: FeatureApproved,
	}}
	ledger, err := crossRunFeatureLedgerBytes(requests)
	if err != nil {
		t.Fatal(err)
	}
	wantLedger := legacyCrossRunJSON(t, map[string]any{"requests": []map[string]any{{
		"description": requests[0].Description, "generation": requests[0].Generation,
		"id": requests[0].ID, "status": requests[0].Status, "title": requests[0].Title,
	}}}, false)
	if !bytes.Equal(ledger, wantLedger) {
		t.Fatalf("typed ledger bytes changed\n got: %s\nwant: %s", ledger, wantLedger)
	}

	bootstrap := &CrossRunBootstrap{
		SourceRun: "experiment-source", SourceGeneration: 3,
		SuccessorISOSHA256: "iso", SuccessorISOSize: 42,
		FeatureLedgerSHA256: "ledger", FeatureLedgerSize: uint64(len(ledger)),
		InheritedRequestIDs: []uint64{7}, GitBaseRef: "source/generation-0003", GitBaseCommit: "commit",
	}
	for _, withHarness := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "harness"}[withHarness], func(t *testing.T) {
			if withHarness {
				source := crossRunHarnessTestIdentity("a", "b")
				destination := crossRunHarnessTestIdentity("c", "d")
				bootstrap.SourceHarnessIdentity = &source
				bootstrap.DestinationHarnessIdentity = &destination
			}
			got, err := crossRunManifestBytes(bootstrap, []byte("handoff λ\n"), len(requests))
			if err != nil {
				t.Fatal(err)
			}
			want := legacyCrossRunManifestBytes(t, bootstrap, []byte("handoff λ\n"), len(requests))
			if !bytes.Equal(got, want) {
				t.Fatalf("typed manifest bytes changed\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func legacyCrossRunManifestBytes(t *testing.T, bootstrap *CrossRunBootstrap, handoff []byte, requestCount int) []byte {
	t.Helper()
	ids := append([]uint64(nil), bootstrap.InheritedRequestIDs...)
	if ids == nil {
		ids = []uint64{}
	}
	identity := crossRunBytesIdentity(handoff)
	value := map[string]any{
		"feature_requests": map[string]any{
			"count": requestCount, "file": CrossRunBootstrapFeatureLedger, "ids": ids,
			"sha256": bootstrap.FeatureLedgerSHA256, "size": bootstrap.FeatureLedgerSize,
		},
		"git_base": map[string]any{"commit": bootstrap.GitBaseCommit, "ref": bootstrap.GitBaseRef},
		"handoff": map[string]any{
			"file": CrossRunBootstrapHandoff, "sha256": identity.SHA256, "size": uint64(len(handoff)),
		},
		"schema_version": crossRunBootstrapLegacySchema,
		"source":         map[string]any{"generation": bootstrap.SourceGeneration, "run": bootstrap.SourceRun},
		"successor_iso": map[string]any{
			"sha256": bootstrap.SuccessorISOSHA256, "size": bootstrap.SuccessorISOSize,
		},
	}
	if bootstrap.DestinationHarnessIdentity != nil {
		var source any
		if bootstrap.SourceHarnessIdentity != nil {
			source = bootstrap.SourceHarnessIdentity.AsJSON()
		}
		value["schema_version"] = crossRunBootstrapSchemaVersion
		value["harness"] = map[string]any{
			"destination": bootstrap.DestinationHarnessIdentity.AsJSON(), "source_generation": source,
		}
	}
	return legacyCrossRunJSON(t, value, true)
}

func legacyCrossRunJSON(t *testing.T, value any, indent bool) []byte {
	t.Helper()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	return unescapeJSONLineSeparators(output.Bytes())
}
