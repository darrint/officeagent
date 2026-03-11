# Agent Instructions for officeagent

This file contains instructions and context for AI coding agents working on this
project.

## Project overview

officeagent is a Go backend server + localhost web UI that gives Windows users
an AI assistant for their email (Office 365), calendar, and local Office files.
See README.md for the full architecture.

## Tech stack

- **Language**: Go
- **LLM**: GitHub Copilot API (Claude)
- **Email/Calendar**: Microsoft Graph API
- **Office files**: Direct file parsing (Go libraries, no COM/Office required)
- **Prompt chaining**: [beads](https://github.com/steveyegge/beads) (`bd`)
- **Storage**: Embedded SQLite
- **Web UI**: Served from the Go binary (embedded assets)
- **Platform target**: Windows (cross-compile from Linux/macOS for release)

## Task tracking

Use `bd` for all task management. Do not use markdown todo lists.

```sh
bd ready          # what to work on next
bd create "Title" # create a new task
bd update <id> --claim  # claim and start a task
bd show <id>      # view task details
```

Install `bd` once if not present:

```sh
go install github.com/steveyegge/beads/cmd/bd@latest
```

Initialize in this repo if not already done:

```sh
bd init
```

## Tasks to do

1. Install beads (`bd init`) and seed initial tasks into it. AND Transfter this
   and any other task lists to beads. (`bd`)
2. Initialize the Go module (`go mod init`).
3. Scaffold the backend server: HTTP server, routes, config loading.
4. Implement Microsoft Graph API client: OAuth2 device-code flow, email read,
   calendar read.
5. Implement local Office file parser: `.docx` (unioffice or go-docx), `.xlsx`
   (excelize).
6. Implement GitHub Copilot API LLM client using beads for prompt chaining and
   context management.
7. Build the web UI (served embedded in the binary).
8. Wire everything together: agent loop, tool dispatch, conversation history in
   SQLite.
9. Cross-compile release binary for Windows (`GOOS=windows go build`).

## Dev environment

```sh
nix develop   # drops into shell with Go, gopls, golangci-lint, sqlite, git
```

Or install Go 1.22+, git, sqlite manually.

## Code conventions

- Standard Go project layout: `cmd/officeagent/`, `internal/`, etc.
- `go vet` and `golangci-lint run` must pass before committing.
- No external runtime dependencies — everything embedded or statically linked.
- Secrets (OAuth tokens, etc.) stored in SQLite, never in source.

## Constraints

- Single binary deployment is a hard requirement.
- No authentication on the web UI — it binds only to localhost.
- LLM provider is GitHub Copilot only (no abstraction layer needed yet).
- Windows is the primary target; Linux/macOS dev builds are fine.
