# Go v1 Astra serving verification

Verified on 2026-09-05 against implementation commit `6bed42f`, branched from
`main` at `9f1b56d`. The preceding AGENTS.md updates are preserved in that base.
This report covers the Go harness only; Python serving defaults remain unchanged.

## Installed app-server evidence

The installed executable reports `codex-cli 0.153.4`. A fresh isolated app-server
authenticated through the existing file-based ChatGPT login returned
`gpt-6-astra` from `model/list`, with reasoning efforts `low`, `medium`, `high`,
`xhigh`, `max`, and `ultra`. It describes `low` as lighter reasoning and advertises
the `priority` service tier as Fast. No literal `light` setting is used.

The installed generated protocol schema supports thread configuration overrides
and returns `reasoningEffort` in `thread/start`. Disposable threads confirmed
the requested model, effort, tier, and isolated permission profiles. The Go client
now checks this acknowledgement before recording successful session/review startup.

A disposable probe using the updated Go `internal/codexapp` client and the actual
Go role configuration completed these remote turns:

| Session | Phase | Model | Effort | Summary | Tier | Result |
| --- | --- | --- | --- | --- | --- | --- |
| Implementor | Planning | `gpt-6-astra` | `high` | `auto` | `priority` | Completed |
| Same implementor thread | Implementation | `gpt-6-astra` | `high` | `auto` | `priority` | Completed |
| Separate reviewer | Review | `gpt-6-astra` | `low` | `auto` | `priority` | Completed |

These sessions used temporary homes and workspaces, declared no guest tools, and
received only a fixed serving-probe prompt. Their processes and temporary probe
source were removed afterward. There was no serving-access failure in these probes.
They do not establish live-generation behavior or full real-model guest-tool
delivery; those boundaries were exercised with synthetic app-server peers.

## Automated verification

All commands below passed. They ran sequentially with these environment settings:

```sh
export GOMAXPROCS=2
export GOCACHE=/tmp/codexos-go-cache
export GOMODCACHE=/tmp/codexos-go-modcache
export GOPATH=/tmp/codexos-go-path
```

```sh
timeout --signal=TERM --kill-after=10s 360s go test -p=1 -parallel=1 ./internal/codexapp ./internal/agent ./internal/operator ./cmd/codexos -count=1 -timeout=300s
timeout --signal=TERM --kill-after=10s 480s go test -p=1 -parallel=1 ./... -count=1 -timeout=300s
timeout --signal=TERM --kill-after=10s 600s go test -race -p=1 -parallel=1 ./... -count=1 -timeout=420s
timeout --signal=TERM --kill-after=10s 180s go vet -p=1 ./...
```

Both complete suites passed across all 12 Go packages. Focused coverage includes
model/effort propagation through planning, implementation, review continuations,
and interviews; role-specific operational provenance; rejection of an unsupported
`light` effort or a substituted thread effort; and the existing review yield,
quiescence, failure, pause/resume, delivery correlation, and process shutdown tests.

The process-leak check compared PID/start-time identities with a baseline of
existing Codex/QEMU/test processes and searched for the unique verification
environment marker inherited by test/probe children. After the successful runs
and probe cleanup, neither check found a remaining new process. Existing live
processes were left untouched.

Opt-in real-image acceptance and live-experiment trials were not run. Feature #3
remains pending. This change provisions no compiler, changes no guest source,
capacity, or readability rules, and introduces no PRISON/2 or async-tool redesign.
See [agent-contract.md](agent-contract.md#serving-settings) for all role settings
and provenance semantics. Live Go cutover remains a separate operator decision.
