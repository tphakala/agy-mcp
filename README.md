# agy-mcp

An MCP (Model Context Protocol) server that wraps the [Antigravity CLI](https://antigravity.google) (`agy`), so any MCP client (Claude Code, Cursor, Cline, and others) can run `agy` prompts, peer reviews, and follow-up turns as native tools.

> Status: early development. The design is being finalized before implementation.

## Why

Driving `agy` from a shell for automation has two recurring problems:

- `agy -p` (print mode) reads stdin even when the prompt is passed with `-p`. If stdin is an open pipe that never closes, it blocks indefinitely. The fix is to always close stdin (`</dev/null`), which is easy to forget.
- A review can run for many minutes. A single blocking call ties up the caller and can exceed a client's tool-call timeout with nothing recoverable.

`agy-mcp` solves both by running `agy` as managed, asynchronous jobs behind a small, typed tool surface, and by capturing output to disk so a run survives a client disconnect or a server restart.

## What it provides

- `agy_run` / `agy_status` / `agy_cancel`: start an `agy` prompt, poll for completion, cancel if needed.
- `list_models`: enumerate available `agy` models.
- `list_sessions`: list known conversations so review threads can be continued.

Session continuation rides `agy`'s own durable conversation store (`--conversation <id>`), so threads survive across calls without keeping a process warm.

## Transports

- **stdio** (default): zero-config, add one line to your MCP client config.
- **Streamable HTTP** (opt-in): `agy-mcp serve --http :PORT` runs the same core as a long-lived daemon for multi-client use.

## Requirements

- The `agy` binary on `PATH` (or configured explicitly).
- Go 1.26+ to build.

## License

MIT. See [LICENSE](LICENSE).
