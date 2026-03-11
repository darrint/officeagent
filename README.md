# officeagent

An AI-powered office productivity agent for Windows. It integrates with your
email, calendar, and local Office files, using an LLM to help you manage and
act on them.

## What it does

- **Email & Calendar**: Reads, summarizes, drafts, and manages email and
  calendar events via the Microsoft Graph API (Office 365 / Outlook.com).
- **Local Office files**: Reads and writes `.docx`, `.xlsx`, `.pptx`, and other
  local files via direct file parsing (no Office installation required).
- **LLM integration**: Uses Claude via the GitHub Copilot API for all AI
  reasoning, prompt chaining, and context management.
- **Web UI**: Serves a local web interface on `localhost` for controlling the
  agent — no external hosting, no authentication required (single-user, local
  machine).

## Architecture

```
┌─────────────────────────────────────────────────┐
│                  officeagent                    │
│                                                 │
│  ┌──────────────┐    ┌───────────────────────┐  │
│  │  Web UI      │    │  Backend Server (Go)  │  │
│  │  (localhost) │◄──►│                       │  │
│  └──────────────┘    │  - HTTP API           │  │
│                      │  - LLM client         │  │
│                      │  - Graph API client   │  │
│                      │  - File parser        │  │
│                      │  - SQLite storage     │  │
│                      └───────────────────────┘  │
│                               │                 │
└───────────────────────────────┼─────────────────┘
                                │
          ┌─────────────────────┼──────────────────┐
          │                     │                  │
          ▼                     ▼                  ▼
  GitHub Copilot API   Microsoft Graph API   Local Files
  (Claude / LLM)       (Email & Calendar)    (.docx, .xlsx, ...)
```

### Key design decisions

| Concern | Decision |
|---|---|
| Language | Go |
| LLM provider | GitHub Copilot API (Claude) |
| Email & Calendar | Microsoft Graph API |
| Office files | Direct file parsing (Go libraries) |
| Prompt chaining | [beads](https://github.com/steveyegge/beads) (`bd`) |
| Storage | Embedded SQLite |
| Deployment | Single binary (backend + embedded web UI assets) |
| Platform | Windows (initially) |
| Auth | None — local-only, single user |

## Deployment

The goal is a **single binary** that:
- Embeds all web UI assets
- Includes the SQLite database engine
- Requires no external dependencies to run

Drop it on a Windows machine, run it, open `http://localhost:<port>` in a
browser.

## Development

### Prerequisites

- [Nix](https://nixos.org/download) with flakes enabled, **or** Go 1.22+,
  git, and sqlite installed manually.

### Dev shell (Nix)

```sh
nix develop
```

This drops you into a shell with Go, gopls, golangci-lint, sqlite, and git
available.

### Build

```sh
go build ./...
```

## Task tracking

This project uses [beads](https://github.com/steveyegge/beads) (`bd`) for
agent-oriented task tracking. Install it once:

```sh
go install github.com/steveyegge/beads/cmd/bd@latest
```

Then use `bd ready` to see what's next.
