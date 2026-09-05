//go:build linux

package bootstrap

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexos/internal/guest"
)

func acceptanceService(t *testing.T, c *Client) (string, *Service) {
	t.Helper()
	run := t.TempDir()
	if e := Provision(run, filepath.Join(t.TempDir(), "store"), "tcc"); e != nil {
		t.Fatal(e)
	}
	path := os.Getenv("CODEXOS_BOOTSTRAP_TCC_TAR")
	if path == "" {
		t.Fatal("set CODEXOS_BOOTSTRAP_TCC_TAR to the prepared pinned archive")
	}
	data, e := os.ReadFile(path)
	if e != nil || Digest(data) != TCCSHA256 {
		t.Fatalf("pinned source: %v", e)
	}
	svc, e := NewService(run, 0, nil, []Asset{{"tcc", TCCSHA256, uint64(len(data))}}, func(id string, o, n uint64) ([]byte, error) { return append([]byte(nil), data[o:o+n]...), nil })
	if e != nil {
		t.Fatal(e)
	}
	svc.client = c
	return run, svc
}
func hostJob(t *testing.T, svc *Service, ctx context.Context, script string, outputs, artifacts []string, extra []guest.SnapshotFile) Result {
	t.Helper()
	files := append([]guest.SnapshotFile{{Path: "seed/bootstrap.sh", Content: []byte(script)}}, extra...)
	snapshot, e := guest.NewSourceSnapshot(files)
	if e != nil {
		t.Fatal(e)
	}
	req := Request{Version: 1, Argv: []string{"/bin/sh", "/inputs/source/seed/bootstrap.sh"}, Outputs: outputs, Artifacts: artifacts}
	if strings.Contains(script, "/inputs/assets/tcc") {
		req.Assets = []AssetRef{{"tcc", TCCSHA256}}
	}
	svc.Activate(ctx)
	defer svc.Deactivate()
	started := time.Now()
	frame, e := svc.HandleRequest(ctx, guest.HostRequest{RequestID: 37, ServiceName: "bootstrap_job", Arguments: [][]byte{mustJSON(req), snapshot.Bytes()}})
	if e != nil {
		t.Fatal(e)
	}
	var result Result
	if e = json.Unmarshal(frame.Payload[4:], &result); e != nil {
		t.Fatal(e)
	}
	if binary.LittleEndian.Uint32(frame.Payload[:4]) != result.Status || frame.RequestID != 37 {
		t.Fatal("host-service correlation/status changed")
	}
	if !validID(result.JobID) {
		t.Fatal("admitted job has no durable provenance identifier")
	}
	sample := result.Diagnostics
	if len(sample) > 512 {
		sample = sample[:512]
	}
	t.Logf("status=%d reason=%s cleaned=%t elapsed=%s controls=%v events=%v diagnostic_bytes=%d prefix=%q", result.Status, result.Reason, result.Cleaned, time.Since(started).Round(time.Millisecond), result.Controls, result.ResourceEvents, len(result.Diagnostics), sample)
	if !result.Cleaned {
		t.Fatalf("cleanup unconfirmed: %+v", result)
	}
	return result
}
func TestRootlessHostServiceTCCAndFailures(t *testing.T) {
	c := acceptanceClient(t)
	run, svc := acceptanceService(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	built := hostJob(t, svc, ctx, `set -eu
cd /work
tar xf /inputs/assets/tcc
cd tinycc
./configure --prefix=/work/installed --with-selinux --extra-cflags=-O0
make -j1 tcc libtcc1.a
./tcc -v
./tcc -B. -run /inputs/source/seed/hello.c
mkdir /work/tool
cp tcc libtcc1.a runmain.o /work/tool/
cp -r include /work/tool/
tar -C /work/tool -cf /work/out/compiler.tar .
gcc -O0 -Wall -Wextra -Werror /inputs/source/seed/limits.c -o /work/out/limits
`, []string{"compiler.tar", "limits"}, nil, []guest.SnapshotFile{
		{Path: "seed/hello.c", Content: []byte("#include <stdio.h>\nint main(void){puts(\"bootstrap-tcc-works\");return 0;}\n")},
		{Path: "seed/limits.c", Content: []byte(acceptanceLimits)},
	})
	if built.Status != 0 || len(built.Artifacts) != 2 {
		t.Fatalf("TCC host job failed: %+v", built)
	}
	compilerID, limitsID := built.Artifacts[0].ID, built.Artifacts[1].ID
	frame, e := svc.HandleRequest(ctx, guest.HostRequest{RequestID: 38, ServiceName: "read_bootstrap_artifact", Arguments: [][]byte{[]byte(compilerID), []byte("0"), []byte("512")}})
	if e != nil || binary.LittleEndian.Uint32(frame.Payload[:4]) != 0 || len(frame.Payload) != 516 {
		t.Fatalf("artifact range: %v", e)
	}
	archive := filepath.Join(run, "generation-0000")
	if e = os.Mkdir(archive, 0700); e != nil {
		t.Fatal(e)
	}
	if e = Freeze(run, archive, 0); e != nil {
		t.Fatal(e)
	}
	// Simulate opening a fresh successor owner from the immutable parent.
	zero := uint64(0)
	resumed, e := NewService(run, 1, &zero, []Asset{svc.assets["tcc"]}, svc.readAsset)
	if e != nil {
		t.Fatal(e)
	}
	resumed.client = c
	reuse := hostJob(t, resumed, ctx, `set -eu
mkdir /work/tool
tar -xf /inputs/artifacts/`+compilerID+` -C /work/tool
/work/tool/tcc -B/work/tool -run /inputs/source/seed/hello.c
printf reused >/work/out/result
`, []string{"result"}, []string{compilerID}, []guest.SnapshotFile{{Path: "seed/hello.c", Content: []byte("#include <stdio.h>\nint main(void){puts(\"retrieved-compiler-runs\");return 0;}\n")}})
	if reuse.Status != 0 {
		t.Fatal("compiler reuse failed")
	}
	oneArchive := filepath.Join(run, "generation-0001")
	_ = os.Mkdir(oneArchive, 0700)
	if e = Freeze(run, oneArchive, 1); e != nil {
		t.Fatal(e)
	}
	rollback, e := NewService(run, 2, &zero, []Asset{svc.assets["tcc"]}, svc.readAsset)
	if e != nil {
		t.Fatal(e)
	}
	rollback.client = c
	denied, e := rollback.HandleRequest(ctx, guest.HostRequest{RequestID: 39, ServiceName: "read_bootstrap_artifact", Arguments: [][]byte{[]byte(reuse.Artifacts[0].ID), []byte("0"), []byte("1")}})
	if e != nil || binary.LittleEndian.Uint32(denied.Payload[:4]) == 0 {
		t.Fatal("rollback authorized later output")
	}
	// Cross-run copying commits before the fresh run becomes visible; it does
	// not automatically enable the destination capability.
	parent := t.TempDir()
	candidate := filepath.Join(parent, "staging")
	destination := filepath.Join(parent, "destination")
	_ = os.Mkdir(candidate, 0700)
	finish, e := Inherit(run, archive, candidate, destination)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(candidate, destination); e != nil {
		t.Fatal(e)
	}
	if e = finish(true); e != nil {
		t.Fatal(e)
	}
	cfg, e := LoadConfig(destination)
	if e != nil || cfg.Enabled {
		t.Fatal("cross-run capability silently enabled")
	}
	if e = Provision(destination, cfg.Storage, cfg.TCCAsset); e != nil {
		t.Fatal(e)
	} // disposable gate fixture only
	inherited, e := NewService(destination, 0, nil, []Asset{svc.assets["tcc"]}, svc.readAsset)
	if e != nil {
		t.Fatal(e)
	}
	inherited.client = c
	reused := hostJob(t, inherited, ctx, "set -eu\nmkdir /work/tool\ntar -xf /inputs/artifacts/"+compilerID+" -C /work/tool\n/work/tool/tcc -v\nprintf inherited >/work/out/result\n", []string{"result"}, []string{compilerID}, nil)
	if reused.Status != 0 {
		t.Fatal("cross-run compiler reuse failed")
	}
	for _, tc := range []struct{ name, script, want string }{
		{"symlink", "ln -s /etc/passwd /work/out/result", "unsafe_output"},
		{"hardlink", "echo x >/work/a; ln /work/a /work/out/result", "unsafe_output"},
		{"fifo", "mkfifo /work/out/result", "unsafe_output"},
		{"noisy", "exec yes diagnostic", "diagnostic_limit"},
		{"oom", "cp /inputs/artifacts/" + limitsID + " /work/limits; chmod +x /work/limits; exec /work/limits memory", "oom"},
		{"pids", "cp /inputs/artifacts/" + limitsID + " /work/limits; chmod +x /work/limits; exec /work/limits pids", "pids_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := []string(nil)
			if tc.name == "oom" || tc.name == "pids" {
				ids = []string{limitsID}
			}
			v := hostJob(t, rollback, ctx, tc.script, []string{"result"}, ids, nil)
			if v.Status == 0 || len(v.Artifacts) != 0 || (tc.want != "" && v.Reason != tc.want) {
				t.Fatalf("failure handling %+v", v)
			}
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		jobCtx, stop := context.WithCancel(ctx)
		timer := time.AfterFunc(time.Second, stop)
		defer timer.Stop()
		v := hostJob(t, rollback, jobCtx, "trap '' TERM; sleep 300 & wait", nil, nil, nil)
		if v.Status == 0 || v.Reason != "cancelled" {
			t.Fatalf("cancel %+v", v)
		}
	})
	if e = rollback.Healthy(); e != nil {
		t.Fatal(e)
	}
	if os.Getenv("CODEXOS_BOOTSTRAP_DEADLINE_ACCEPTANCE") == "1" {
		t.Run("independent_deadline", func(t *testing.T) {
			started := time.Now()
			v := hostJob(t, rollback, ctx, "trap '' TERM; sleep 300 & wait", nil, nil, nil)
			if v.Status == 0 || time.Since(started) > 195*time.Second {
				t.Fatalf("deadline %+v", v)
			}
		})
	}
}

const acceptanceLimits = `#define _POSIX_C_SOURCE 200809L
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <signal.h>
#include <sys/wait.h>
#include <errno.h>
int main(int argc,char **argv){
 if(argc!=2)return 2;
 if(!strcmp(argv[1],"memory")){
  for(int i=0;i<768;i++){volatile unsigned char *p=malloc(1024*1024);if(!p)return 3;for(int j=0;j<1024*1024;j+=4096)p[j]=1;}
  return 0;
 }
 if(!strcmp(argv[1],"pids")){
  pid_t children[128];int n=0,limited=0;
  for(;n<128;n++){pid_t p=fork();if(p<0){limited=errno==EAGAIN;break;}if(!p){sleep(30);_exit(0);}children[n]=p;}
  for(int i=0;i<n;i++)kill(children[i],SIGKILL);
  for(int i=0;i<n;i++)waitpid(children[i],NULL,0);
  printf("forked=%d limited=%d\n",n,limited);return limited?3:0;
 }
 return 2;
}
`
