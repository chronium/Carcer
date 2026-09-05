// codexos-bootstrap is the fixed dedicated-account worker, not a guest CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"codexos/internal/bootstrap"
)

func main() {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "__") && os.Args[1] != "__worker" {
		if e := bootstrap.Helper(os.Args[1:], os.Stdout); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	}
	u, e := user.Current()
	if e != nil || u.Username != bootstrap.Account || os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "requires the dedicated rootless codexos-bootstrap account")
		os.Exit(1)
	}
	// sudo and systemd-run --scope preserve the caller's cwd. It may be a
	// harness checkout that this dedicated account must not be able to enter.
	if e = os.Chdir(u.HomeDir); e != nil {
		fmt.Fprintln(os.Stderr, "cannot enter bootstrap account home:", e)
		os.Exit(1)
	}
	env := []string{"HOME=" + u.HomeDir, "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(os.Geteuid()), "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + strconv.Itoa(os.Geteuid()) + "/bus", "GOMAXPROCS=2", "GOMEMLIMIT=192MiB"}
	if len(os.Args) == 1 {
		executable, e := os.Executable()
		if e == nil {
			e = syscall.Exec("/usr/bin/systemd-run", []string{"systemd-run", "--user", "--scope", "--quiet", "--slice=" + bootstrap.JobSlice, executable, "__worker"}, env)
		}
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if len(os.Args) != 2 || os.Args[1] != "__worker" {
		fmt.Fprintln(os.Stderr, "worker accepts no operator arguments")
		os.Exit(1)
	}
	os.Clearenv()
	for _, kv := range env {
		p := strings.SplitN(kv, "=", 2)
		_ = os.Setenv(p[0], p[1])
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	e = bootstrap.ServeWorker(ctx, os.Stdin, os.Stdout, bootstrap.WorkerOptions{Directory: filepath.Join(u.HomeDir, "state"), Slice: bootstrap.JobSlice})
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
