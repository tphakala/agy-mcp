# AGENTS.md

Guidance for coding agents working in this repository. The README is the user-facing contract; this
file covers how the code is organised and the conventions a change is expected to follow.

## What this is

agy-mcp is a Go MCP server that wraps the Antigravity CLI (`agy`). Each tool call that runs a prompt
becomes a disk-backed job: the server spawns a detached supervisor (`agy-mcp run-job <jobDir>`), which
runs `agy -p --output-format stream-json` and mirrors its output into the job directory. Status is
derived from those files, so a job survives a server restart.

Module path: `github.com/tphakala/agy-mcp/v2`. Go version: see `go.mod`.

## Layout

| Path | Role |
|---|---|
| `main.go` | Entry point. Subcommands `run-job`, `wait-job`, `hook-wait`, `doctor`; otherwise serves MCP over stdio or HTTP. |
| `internal/mcptools` | MCP tool definitions. The `jsonschema` struct tags in `tools.go` are the tool descriptions clients see. |
| `internal/manager` | Job lifecycle: request normalization, spawning, status derivation (`status.go`), cancel, models, agents, sessions. |
| `internal/supervisor` | The per-job process that runs agy and writes the job directory. |
| `internal/jobstore` | On-disk job state: `meta.json`, file-name constants, exit-code sentinels. |
| `internal/streamjson` | Decoder for agy's stream-json events. |
| `internal/proc` | Process-group and session primitives, per platform (`proc_posix.go` is `linux \|\| darwin`, `proc_windows.go`). |
| `internal/agyver` | agy version parsing and the minimum supported version (`agyver.Required`). |
| `internal/testutil` | Test doubles: fake agy and fake supervisor scripts. |
| `rules/` | ruleguard matchers used by golangci-lint (build tag `ruleguard`). |

## Build, test, lint

```bash
go build ./...
go vet ./...
go test ./... -race
golangci-lint run                 # uses .golangci.yaml; CI pins the version in .github/workflows/ci.yml
go build -tags ruleguard ./rules/ # a broken rule otherwise compiles clean and silently disables ruleguard
```

CI runs build, vet and tests on Linux, macOS and Windows (Windows without `-race`) and lint on Linux.
Run the full suite before proposing a change; several tests exercise real child processes and timing.

## The job directory contract

Every package that touches a job directory uses the constants in `internal/jobstore` (`MetaFile`,
`OutFile`, `ErrFile`, `ExitCodeFile`, `ResultFile`, `ProgressFile`, `CancelFile`), never string
literals. The supervisor writes `result.json` only after agy has been reaped, and writes the
`exit_code` sentinel last, because the manager treats the sentinel as the completion signal. Keep that
order when changing the supervisor.

Exit-code sentinels (`jobstore.ExitTimeout`, `ExitSpawnFail`, `ExitSIGINT`, `ExitSIGTERM`) carry
meaning beyond agy's own exit code; `statusFromExitCode` in `internal/manager/status.go` interprets
them.

## Public contract rules

- `failure_reason` is a closed set (the `Reason*` constants in `internal/manager/status.go`). Adding,
  removing or redefining a value is a contract change: update the constant comment, the `FailureReason`
  schema text in `internal/mcptools/tools.go`, and the README together.
- Tool descriptions and field schemas in `tools.go` and the README describe the same behaviour. When
  one changes, grep for the claim and update every copy.
- The minimum agy version lives in `agyver.Required`. Raising it is deliberate: update its test and
  the README requirements line.

## Conventions

- **Claims about agy's behaviour need evidence.** A comment or doc sentence describing what agy does
  (its stderr wording, exit codes, stream shape) states how it is known: `MEASURED against agy X.Y.Z`,
  or a citation of agy's changelog. If it was not measured, do not write it as fact. Several past
  defects were confident but untested sentences about agy.
- **Counts go stale.** Prefer an invariant ("every caller routes through X") to a count ("the two
  callers"). If you do write a count, check it against the code first.
- **Fail safe on classification.** Status derivation biases toward precision: a missed stderr notice
  leaves the prior reporting in place, while a false match downgrades a real answer. Keep matchers
  line-scoped and conservative, and keep the text a run produced (relabel, never empty it).
- **Comments cite issues** (`issue #173`) when they explain why a branch exists.
- **Platform splits** use build tags and file suffixes: `_posix` files carry `//go:build linux ||
  darwin`, Windows code lives in `_windows.go`. The fake agy and fake supervisor are shell scripts, so
  tests that spawn them live in `*_posix_test.go`; code paths shared with Windows also need a test that
  runs there.
- **Tests must be able to fail.** For each new test, name the production line whose removal should
  turn it red, remove it, and watch the test fail on an assertion. A test that starts a real supervisor
  should wait for its exit-code sentinel before `t.TempDir` cleanup (see `deferJobDone`).
- **gocritic `hugeParam` is off on purpose:** `StartRequest`, `config.Config` and `jobstore.Meta` are
  passed by value so callees mutate their own copy.
- **Prose style:** no em or en dashes in code, comments, docs or commit messages.

## Commits and pull requests

- Conventional commit subjects: `fix:`, `feat:`, `refactor:`, `docs:`, `test(scope):`, `chore(deps):`.
- Describe what the change does and why, and how it was verified (tests, vet, lint).
- Local design notes and plans are not committed (`.gitignore` covers `/plans/`, `/specs/`,
  `*-design.md`, `*-plan.md`).
