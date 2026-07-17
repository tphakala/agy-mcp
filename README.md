# agy-mcp

[![CI](https://github.com/tphakala/agy-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/agy-mcp/actions/workflows/ci.yml)
[![CodeQL](https://github.com/tphakala/agy-mcp/actions/workflows/codeql.yml/badge.svg)](https://github.com/tphakala/agy-mcp/actions/workflows/codeql.yml)
[![Version](https://img.shields.io/github/v/tag/tphakala/agy-mcp?label=version&sort=semver)](https://github.com/tphakala/agy-mcp/tags)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/agy-mcp.svg)](https://pkg.go.dev/github.com/tphakala/agy-mcp)
[![Go Report Card](https://goreportcard.com/badge/github.com/tphakala/agy-mcp)](https://goreportcard.com/report/github.com/tphakala/agy-mcp)
[![License: MIT](https://img.shields.io/github/license/tphakala/agy-mcp)](LICENSE)

An MCP (Model Context Protocol) server that wraps the [Antigravity CLI](https://antigravity.google) (`agy`), so any MCP client (Claude Code, Cursor, Cline, and others) can run `agy` prompts, peer reviews, and follow-up turns as native tools.

> Status: feature complete (stdio and HTTP transports, async and sync job lifecycle, model and session discovery, cross-platform builds) and verified against a live agy (1.0.11).

## Why

Driving `agy` from a shell for automation has two recurring problems:

- `agy -p` (print mode) reads stdin even when the prompt is passed with `-p`. If stdin is an open pipe that never closes, it blocks indefinitely. The fix is to always close stdin (`</dev/null`), which is easy to forget.
- A review can run for many minutes. A single blocking call ties up the caller and can exceed a client's tool-call timeout with nothing recoverable.

`agy-mcp` solves both by running `agy` as managed, asynchronous jobs behind a small, typed tool surface, and by capturing output to disk so a run survives a client disconnect or a server restart.

## What it provides

- `agy_run` / `agy_status` / `agy_cancel`: start an `agy` prompt, poll for completion, cancel if needed.
- `agy_run_sync`: start a prompt and wait for it inline (bounded, with MCP progress notifications); returns the `job_id` to poll if it outlives the wait cap.
- `agy_wait`: block until an already-started job finishes (bounded, with MCP progress notifications); one call replaces an `agy_status` poll loop.
- `list_models`: enumerate available `agy` models.
- `list_sessions`: list known conversations so review threads can be continued.

Session continuation rides `agy`'s own durable conversation store (`--conversation <id>`), so threads survive across calls without keeping a process warm.

Two transports run the same core:

- **stdio** (default): zero-config, one line in your MCP client config.
- **Streamable HTTP** (opt-in): a long-lived, loopback-only daemon for multi-client use. See [HTTP mode](#http-mode).

## Requirements

- The `agy` binary on `PATH` (or configured explicitly via `AGY_MCP_AGY_PATH`). agy 1.0.9 or newer is recommended (see the continuation note below).
- Go 1.26+ to build.
- The server builds and runs on Linux, macOS, and Windows. Job supervision (running agy as managed jobs via `agy_run` / `agy_run_sync` / `agy_status` / `agy_cancel`) is implemented on **Linux** and **macOS** (process groups, SIGTERM cancel, an advisory flock) and on **Windows** (Job Objects, `OpenProcess` + process creation time, `LockFileEx`); stdio/HTTP serving, `list_models`, and `list_sessions` work identically everywhere.

  Windows behaves the same as Linux with two documented differences:
  - **Cancel and hard timeout are hard kills.** Linux sends agy `SIGTERM` and waits a 10s grace before `SIGKILL`; Windows has no equivalent signal for a detached process, so `TerminateJobObject` ends the whole process tree immediately with no flush window.
  - **Cancel is delivered via a sentinel file, not a signal.** The manager writes a `cancel` file into the job directory and the supervisor polls for it (a few hundred ms), since Windows cannot signal an arbitrary process. Liveness uses the process creation time (an absolute wall-clock value) to defend against PID recycling, which subsumes the Linux boot-id check across reboots.

> Note: every job spawns a fresh `agy` process, which on startup launches whatever MCP servers are configured in agy's own `mcp_config.json`. Peer-review and automation runs usually do not need those servers, and a slow or hanging one adds latency to every job. agy 1.0.7+ bounds this with a per-server launch `timeout` (set `-1` to disable it). If startup is slow, give the unneeded servers a `timeout` in agy's `mcp_config.json`, or point agy at a trimmed config.

> Note (continuation): a follow-up turn runs `agy --conversation <id> -p <prompt>`, and the supervisor captures agy's stdout verbatim as the job result with no post-processing. agy 1.0.9 fixed a print-mode resumption bug where a resumed `-p` dumped the entire conversation transcript instead of only the new turn; on earlier builds (through 1.0.7) every continued `conversation_id` / `continue_latest` result was polluted with the full prior transcript. On 1.0.9+ the result is just the new response. Verified clean end-to-end against agy 1.0.11.

> Note (auth): agy-mcp passes its own process environment through to every spawned `agy`, so agy's normal OAuth credentials are used by default. For headless or daemon deployments (HTTP mode, cron) where no browser is available, set `USE_ADC=1` in the server's environment to have agy authenticate via Google Application Default Credentials instead of the interactive sign-in flow (agy 1.0.11+). No agy-mcp flag or code change is needed; unset it to fall back to the other sign-in methods.

## Install

```bash
go install github.com/tphakala/agy-mcp@latest
```

## Use with Claude Code (stdio)

```bash
claude mcp add agy -- agy-mcp
```

Or add to your MCP client config:

```json
{
  "mcpServers": {
    "agy": { "command": "agy-mcp" }
  }
}
```

## Tools

- `agy_run(prompt, model?, dirs?, conversation_id?, continue_latest?, cwd?, timeout?)` -> `{ job_id, conversation_id?, state }`
- `agy_run_sync(prompt, model?, dirs?, conversation_id?, continue_latest?, cwd?, timeout?, wait?)` -> `{ job_id, state, elapsed, result?, error?, conversation_id?, note? }`
- `agy_status(job_id)` -> `{ state, elapsed, result?, error?, conversation_id? }`
- `agy_wait(job_id, wait?)` -> `{ job_id, state, elapsed, result?, error?, conversation_id?, note? }`
- `agy_cancel(job_id)` -> `{ state }`
- `list_models()` -> `{ models }`
- `list_sessions(dir?)` -> `{ sessions }`

A fresh `agy_run` (no `conversation_id`, no `continue_latest`) starts with an empty
`conversation_id`; agy assigns one as the run proceeds, and `agy_status` reports it once the
run completes, so the thread can be continued later. To keep that capture unambiguous, fresh
runs sharing a `cwd` are serialized: while one fresh run is active, a second fresh run in the
same directory is refused (`agy_run` returns a conflict error rather than queuing it), so run
them in separate directories or retry once the first finishes. Runs in different directories,
and runs continuing distinct conversations, still run concurrently up to the configured cap.
The gate that enforces this is rebuilt at startup from jobs whose supervisor outlived a server
restart, so serialization holds across restarts.

This serialization holds across separate `agy-mcp` processes too, which matters in stdio mode,
where each MCP client session spawns its own process sharing one `AGY_MCP_STATE_DIR`. A per-key
advisory lock (`flock` on a file under `<state-dir>/locks/`) serializes same-`cwd`/same-conversation
runs across processes, so two sibling sessions cannot start a conflicting run concurrently. The
global concurrency cap, by contrast, is enforced per process: with N client sessions each capped
at the configured limit, up to N times that many distinct, non-conflicting runs can be in flight.
The lock files are tiny and left in place by design (unlinking a `flock` file races), so the
`locks/` directory keeps one empty file per distinct directory or conversation ever locked.

Two caveats. `AGY_MCP_STATE_DIR` must live on a local filesystem that supports `flock` (not NFS,
where `flock` may be a no-op or fail); a run is refused rather than started if the lock cannot be
taken, so cross-process exclusion can never silently lapse. And across a server restart there is a
brief window, while the manager process is down, before it re-takes the locks for jobs whose
supervisor outlived it; a sibling process that starts the same-`cwd` run during that window is not
blocked. The in-process gate is always restored at startup, so this gap is limited to the restart
window itself.

## Completion wake for Claude Code

Claude Code does not surface MCP server notifications to the model: progress notifications are UI-only, and there is no server-initiated push that can wake the model when an async job finishes. Polling `agy_status` after `agy_run` is the protocol-level baseline. agy-mcp ships a hook bridge that turns job completion into a real wake instead, built on Claude Code's `asyncRewake` hook mechanism: a background PostToolUse hook that exits with code 2 wakes the model with the hook's stderr as a system reminder.

Add to your Claude Code `settings.json`:

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "mcp__agy__agy_run(_sync)?",
        "hooks": [
          { "type": "command", "command": "agy-mcp hook-wait", "asyncRewake": true, "timeout": 3700 }
        ]
      }
    ]
  }
}
```

How it behaves:

- After every `agy_run`, the hook waits (in the background, off the session's critical path) for the job's completion sentinel and then wakes Claude with a one-line message naming the job id and final state, so the model calls `agy_status` exactly once, when the result is actually ready.
- `agy_run_sync` calls that returned their result inline produce no wake; a sync call that overran its wait cap (and so returned a still-running job) gets the same completion wake as `agy_run`.
- On timeout (default 1h, `-timeout` to change) the hook still wakes Claude, reporting the job as still running, so a long job is never silently lost. On any internal error the hook exits 0 silently; it can never disrupt the tool call it observes. The hook-level `timeout` in `settings.json` (3700 above) must exceed hook-wait's own `-timeout` (1h by default), so hook-wait's internal timeout always fires first and reports the wake itself; if the outer `timeout` is equal or lower, Claude Code kills the hook process first and the exit-2 wake is lost, silently defeating the "never silently lost" guarantee.
- The matcher entry name `agy` is the server name from `claude mcp add agy`; adjust both if you registered the server under a different name.
- Claude Code runs hooks with the user's shell environment, not the MCP server entry's own env block, so if the server is registered with a custom `AGY_MCP_STATE_DIR` the same value must also be exported in the shell (or passed inline in the hook command) or hook-wait will silently watch the wrong state directory and never wake.

Two related subcommands, useful beyond Claude Code:

- `agy-mcp wait-job [-timeout 1h] <job_id>` blocks until the job is terminal and prints the final state word (`done`, `failed`, or `cancelled`) to stdout. Exit codes: 0 terminal, 1 error, 2 usage, 3 timeout, 130 interrupted. It needs only the job state directory, not the agy binary, so it works in minimal environments.
- `agy-mcp hook-wait [-timeout 1h]` is the hook entrypoint described above: it reads the PostToolUse payload from stdin, so it is not useful to invoke by hand, but it is a single self-contained binary call, no shell wrapper or jq required, and it works on Linux, macOS, and Windows.

MCP clients other than Claude Code get the same no-polling benefit in-protocol: call `agy_wait` with the `job_id` returned by `agy_run` and the tool blocks (bounded by `wait`, default 2m, max 10m) until the job finishes.

## HTTP mode

```bash
agy-mcp -http 127.0.0.1:8765
```

HTTP mode is opt-in and only accepts a loopback bind address (`localhost`, `127.0.0.1`, or `::1`). A non-loopback address (including `:8765`, which binds all interfaces) is refused at startup, so it cannot be accidentally exposed.

Two extra hardening layers are always available:

- **Origin/CSRF protection** is always on. A state-changing cross-origin browser request (one carrying a cross-site `Sec-Fetch-Site` or a mismatched `Origin`) is rejected with `403`. Normal non-browser MCP clients (Claude Code, Cursor, the go-sdk client) send no `Origin` header and are unaffected.
- **Optional bearer token.** Set `-http-token <token>` (or `AGY_MCP_HTTP_TOKEN`) to require `Authorization: Bearer <token>` on every request; a missing or wrong token gets `401`. The flag overrides the env var. Off by default, so the bare loopback mode stays unauthenticated.

```bash
agy-mcp -http 127.0.0.1:8765 -http-token "$(openssl rand -hex 32)"
```

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `AGY_MCP_AGY_PATH` | `agy` on PATH | path to the agy binary |
| `AGY_MCP_STATE_DIR` | `$XDG_STATE_HOME/agy-mcp` | job state directory |
| `AGY_MCP_DEFAULT_MODEL` | agy default | default model |
| `AGY_MCP_HTTP_TOKEN` | (none) | optional bearer token for HTTP mode; empty = unauthenticated |

## Development

```bash
go build ./...
go test ./... -race
golangci-lint run
```

CI builds and vets on Linux, macOS, and Windows; runs the race-enabled test suite on Linux and macOS; and runs the test suite (without the race detector) on Windows.

## License

MIT. See [LICENSE](LICENSE).
