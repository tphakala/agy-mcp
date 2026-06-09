# agy-mcp

An MCP (Model Context Protocol) server that wraps the [Antigravity CLI](https://antigravity.google) (`agy`), so any MCP client (Claude Code, Cursor, Cline, and others) can run `agy` prompts, peer reviews, and follow-up turns as native tools.

> Status: feature complete (stdio and HTTP transports, async and sync job lifecycle, model and session discovery, cross-platform builds) and verified against a live agy (1.0.6). Latest release: v1.0.0.

## Why

Driving `agy` from a shell for automation has two recurring problems:

- `agy -p` (print mode) reads stdin even when the prompt is passed with `-p`. If stdin is an open pipe that never closes, it blocks indefinitely. The fix is to always close stdin (`</dev/null`), which is easy to forget.
- A review can run for many minutes. A single blocking call ties up the caller and can exceed a client's tool-call timeout with nothing recoverable.

`agy-mcp` solves both by running `agy` as managed, asynchronous jobs behind a small, typed tool surface, and by capturing output to disk so a run survives a client disconnect or a server restart.

## What it provides

- `agy_run` / `agy_status` / `agy_cancel`: start an `agy` prompt, poll for completion, cancel if needed.
- `agy_run_sync`: start a prompt and wait for it inline (bounded, with MCP progress notifications); returns the `job_id` to poll if it outlives the wait cap.
- `list_models`: enumerate available `agy` models.
- `list_sessions`: list known conversations so review threads can be continued.

Session continuation rides `agy`'s own durable conversation store (`--conversation <id>`), so threads survive across calls without keeping a process warm.

## Transports

- **stdio** (default): zero-config, add one line to your MCP client config.
- **Streamable HTTP** (opt-in): `agy-mcp -http 127.0.0.1:8765` runs the same core as a long-lived daemon for multi-client use. The bind is restricted to loopback (`localhost`, `127.0.0.1`, `::1`); a non-loopback address is refused at startup. Cross-origin browser requests are rejected (Origin/CSRF protection), and an optional bearer token (`-http-token`) can require authentication.

## Requirements

- The server builds and runs on Linux, macOS, and Windows. Job supervision (running agy as managed jobs via `agy_run` / `agy_run_sync` / `agy_status` / `agy_cancel`) relies on process groups, `/proc`, and the kernel boot id, so it runs on Linux only; on macOS and Windows those tools return a clear "job supervision is only supported on Linux" error, while stdio/HTTP serving, `list_models`, and `list_sessions` work everywhere.
- The `agy` binary on `PATH` (or configured explicitly).
- Go 1.26+ to build.

## License

MIT. See [LICENSE](LICENSE).

## Install

```bash
go install github.com/tphakala/agy-mcp@latest
```

Requires the `agy` binary on `PATH` (or set `AGY_MCP_AGY_PATH`).

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
restart, so the cap and serialization hold across restarts.

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
