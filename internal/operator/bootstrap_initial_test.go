package operator

import (
	"bytes"
	"codexos/internal/bootstrap"
	"codexos/internal/experiment"
	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bootstrapOperatorWorker(failure string) int {
	var n uint32
	if binary.Read(os.Stdin, binary.BigEndian, &n) != nil || n > 1<<20 {
		return 2
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(os.Stdin, b); e != nil {
		return 2
	}
	result := bootstrap.Result{Status: 0, Reason: "available", Cleaned: true}
	if _, e := os.Stat(failure); e == nil {
		result.Status = 2
		result.Reason = "unavailable"
	}
	b, _ = json.Marshal(map[string]any{"result": result, "outputs": []any{}})
	if binary.Write(os.Stdout, binary.BigEndian, uint32(len(b))) != nil {
		return 2
	}
	if _, e := os.Stdout.Write(b); e != nil {
		return 2
	}
	return 0
}

func TestRunnerProvisionsInheritedBootstrapBeforeBoot(t *testing.T) {
	tarPath := os.Getenv("CODEXOS_BOOTSTRAP_TCC_TAR")
	if tarPath == "" {
		t.Skip("set CODEXOS_BOOTSTRAP_TCC_TAR for pinned-input operator acceptance")
	}
	upstream, e := os.ReadFile(tarPath)
	if e != nil || bootstrap.Digest(upstream) != bootstrap.TCCSHA256 {
		t.Fatal("incorrect pinned TCC input", e)
	}
	root, e := os.MkdirTemp("/tmp", "co-ib-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	source := filepath.Join(root, "source")
	if e = os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	assets := filepath.Join(root, "assets")
	if e = os.MkdirAll(filepath.Join(assets, "tcc"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(assets, "tcc", "source.tar"), upstream, 0400); e != nil {
		t.Fatal(e)
	}
	// Source fixtures may use storage primitives. Every destination grant below
	// goes through CLI parsing, the operator runner and initial runtime validation.
	if e = bootstrap.Provision(source, filepath.Join(root, "artifacts"), "tcc"); e != nil {
		t.Fatal(e)
	}
	cfg, e := bootstrap.LoadConfig(source)
	if e != nil {
		t.Fatal(e)
	}
	storage, e := bootstrap.LockStorage(*cfg)
	if e != nil {
		t.Fatal(e)
	}
	snapshot, e := guest.EncodeSourceSnapshot([]guest.SnapshotFile{{Path: "seed/kernel.c", Content: []byte("fixture")}})
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("boot runtime")
	id := bootstrap.Digest(data)
	refs := bootstrap.NewReferences(*cfg, 0)
	now := time.Now().UTC()
	manifest := bootstrap.Manifest{Version: 1, RunID: cfg.RunID, ID: bootstrap.Digest([]byte("source job")), Image: bootstrap.Image, ImageID: bootstrap.ImageID, TCCCommit: bootstrap.TCCCommit, Request: bootstrap.Request{Version: 1, Argv: []string{"true"}, Outputs: []string{"runtime"}}, SnapshotSHA256: bootstrap.Digest(snapshot), SourceContentBytes: 65536, Limits: bootstrap.Baseline(), Started: now, Finished: now, Result: bootstrap.Result{Status: 0, Cleaned: true, Artifacts: []bootstrap.Artifact{{ID: id, Name: "runtime", Size: int64(len(data))}}}}
	if e = storage.Publish(context.Background(), manifest, []bootstrap.Input{{Path: "runtime", Data: data}}, &refs); e != nil {
		t.Fatal(e)
	}
	storage.Close()
	hardware, e := qemu.TestHardwareProfile.Manifest("disposable bootstrap")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = experiment.WriteCompletedArchive(source, experiment.CompletedArchive{Generation: 0, Transition: "initial", Hardware: hardware, BootISO: []byte("boot"), Handoff: "inherited bootstrap", SourceSnapshot: snapshot, KernelELF: []byte("elf"), SuccessorISO: []byte("successor")}); e != nil {
		t.Fatal(e)
	}
	repo := filepath.Join(root, "repo")
	initializeDisposableGitRepository(t, repo)
	recorder, e := provenance.NewGenerationGitRecorder(repo, source, "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = recorder.Reconcile(); e != nil {
		t.Fatal(e)
	}
	qemuExecutable := buildDisposableRunnerQEMU(t)
	t.Setenv("CODEXOS_DISPOSABLE_BOOTSTRAP_ARTIFACT", id)
	initial := filepath.Join(source, "generation-0000", "successor", "codexos.iso")
	for _, tc := range []struct {
		name, asset string
		fail        bool
	}{{"disabled", "", false}, {"enabled", "tcc", false}, {"wrong-pin", "missing", false}, {"worker-failure", "tcc", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(root, tc.name)
			marker := filepath.Join(root, tc.name+"-read")
			t.Setenv("CODEXOS_DISPOSABLE_BOOTSTRAP_READ_MARKER", marker)
			disabled := ""
			if tc.asset == "" {
				disabled = "1"
			}
			t.Setenv("CODEXOS_DISPOSABLE_BOOTSTRAP_DISABLED", disabled)
			failure := filepath.Join(root, tc.name+"-worker-failure")
			if tc.fail {
				if e = os.WriteFile(failure, []byte("fail"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			configuration := runnerConfiguration{live: experiment.LiveRunOptions{QEMUExecutable: qemuExecutable, HardwareProfile: qemu.TestHardwareProfile, ReadyTimeout: 2 * time.Second, BootstrapClient: &bootstrap.Client{Command: []string{os.Args[0], "--bootstrap-operator-worker", failure}}}}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var output bytes.Buffer
			command := NewCommand(false, func(ctx context.Context, o Options) error {
				return runWithIOConfigured(ctx, o, strings.NewReader("status\nquit\n"), &output, configuration)
			})
			args := []string{"--run-directory", dest, "--initial-iso", initial, "--inherit-from-run", source, "--inherit-from-generation", "0", "--git-repository", repo, "--git-base-ref", "source/generation-0000", "--provided-assets", assets, "--plain"}
			if tc.asset != "" {
				args = append(args, "--provision-inherited-bootstrap", tc.asset)
			}
			command.SetArgs(args)
			command.SetContext(ctx)
			err := command.Execute()
			wantSuccess := tc.asset == "" || tc.asset == "tcc" && !tc.fail
			if (err == nil) != wantSuccess {
				t.Fatalf("operator: %v output=%s", err, output.String())
			}
			destinationConfig, e := bootstrap.LoadConfig(dest)
			if e != nil || destinationConfig == nil {
				t.Fatal(e)
			}
			if destinationConfig.Enabled != (wantSuccess && tc.asset != "") {
				t.Fatal("destination enabled without successful explicit provisioning")
			}
			_, readErr := os.Stat(marker)
			if (readErr == nil) != wantSuccess {
				t.Fatalf("boot occurred at incorrect boundary: %v", readErr)
			}
			if tc.fail {
				// Retry the persisted unstarted destination without inheriting/publishing it again.
				if e = os.Remove(failure); e != nil {
					t.Fatal(e)
				}
				options := Options{RunDirectory: dest, InitialISO: initial, GitRepository: repo, GitBaseRef: "source/generation-0000", ProvidedAssets: assets, ProvisionInheritedBootstrap: "tcc"}
				if e = runWithIOConfigured(ctx, options, strings.NewReader("quit\n"), &output, configuration); e != nil {
					t.Fatalf("retry: %v", e)
				}
				if _, e = os.Stat(marker); e != nil {
					t.Fatal("retry did not read inherited artifact before ready", e)
				}
			}
			if wantSuccess || tc.fail {
				// Reopening the stopped process must not recreate the initial grant
				// window after generation zero has already started.
				reopened, e := experiment.NewLiveCodexOSRun(dest, configuration.live)
				if e != nil {
					t.Fatal(e)
				}
				if e = reopened.ProvisionInitialBootstrap(ctx, "tcc", initial); e == nil || !strings.Contains(e.Error(), "generation start record") {
					t.Fatalf("reprovision after boot: %v", e)
				}
				reopened.Close()
			}
			if tc.name == "wrong-pin" {
				options := Options{RunDirectory: dest, InitialISO: initial, GitRepository: repo, GitBaseRef: "source/generation-0000", ProvidedAssets: assets, ProvisionInheritedBootstrap: "tcc"}
				for _, name := range []string{"bad-refs", "partial", "wrong-image"} {
					t.Run(name, func(t *testing.T) {
						path := filepath.Join(dest, "bootstrap-inherited.json")
						original, e := os.ReadFile(path)
						if e != nil {
							t.Fatal(e)
						}
						opts := options
						switch name {
						case "bad-refs":
							if e = os.WriteFile(path, []byte("{}"), 0600); e != nil {
								t.Fatal(e)
							}
							defer os.WriteFile(path, original, 0600)
						case "partial":
							path = filepath.Join(dest, ".generation-incomplete")
							if e = os.Mkdir(path, 0700); e != nil {
								t.Fatal(e)
							}
							defer os.Remove(path)
						case "wrong-image":
							opts.InitialISO = filepath.Join(root, "wrong.iso")
							if e = os.WriteFile(opts.InitialISO, []byte("wrong"), 0600); e != nil {
								t.Fatal(e)
							}
						}
						if e = runWithIOConfigured(ctx, opts, strings.NewReader("quit\n"), &output, configuration); e == nil {
							t.Fatal("invalid initial destination provisioned")
						}
						c, e := bootstrap.LoadConfig(dest)
						if e != nil || c.Enabled {
							t.Fatalf("failed validation enabled destination: %+v %v", c, e)
						}
						if _, e = os.Stat(marker); !os.IsNotExist(e) {
							t.Fatal("invalid destination booted")
						}
					})
				}
			}

		})
	}
}
