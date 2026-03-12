# Agent Instructions for officeagent

This file contains instructions and context for AI coding agents working on this
project.

## Project overview

officeagent is a Go backend server + localhost web UI that gives Windows users
an AI assistant for their email (Office 365), calendar, and local Office files.
See README.md for the full architecture.

## Tech stack

- **Language**: Go
- **LLM**: GitHub Models API (default: `gpt-4o-mini`); Copilot API migration planned for Claude access
- **Email/Calendar**: Microsoft Graph API
- **Office files**: Via Microsoft Graph API and Windows-specific APIs (direct Go parsing explicitly deferred)
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

`bd` is provided by the Nix dev shell (built from source at v0.59.0 — the
nixpkgs package was stale). If working outside Nix, install manually:

```sh
go install github.com/steveyegge/beads/cmd/bd@v0.59.0
```

Initialize in this repo if not already done:

```sh
bd init
```

## Dev environment

```sh
nix develop   # drops into shell with Go, gopls, golangci-lint, sqlite, git, dolt, bd, gh
```

Or install Go 1.22+, git, sqlite manually.

## LLM setup

The LLM client uses the GitHub Models API by default.

**Environment variables:**
- `GITHUB_TOKEN` — GitHub OAuth token with `copilot` scope. Get it via:
  ```sh
  gh auth login --scopes copilot
  export GITHUB_TOKEN=$(gh auth token)
  ```
- `OFFICEAGENT_LLM_MODEL` — model ID (default: `gpt-4o-mini`)

Keep these in an uncommitted `tokens.sh` (already in `.gitignore`) for local dev:
```sh
export GITHUB_TOKEN=$(gh auth token)
export OFFICEAGENT_LLM_MODEL=gpt-4o-mini
```

**Two different APIs with different model availability:**

| API | Base URL | Auth | Claude? |
|-----|----------|------|---------|
| GitHub Models | `models.inference.ai.azure.com` | PAT as Bearer | No |
| GitHub Copilot | `api.githubcopilot.com` | OAuth token (copilot scope) as Bearer | Yes |

The Copilot OAuth token works **directly** as a Bearer token against
`api.githubcopilot.com` — **no token exchange step is required**.
The app currently uses the Copilot API with `claude-sonnet-4.6` as default.

## Known gotchas

**Azure OAuth:**
- Auth code + PKCE works. Device code flow is blocked by enterprise Conditional
  Access policies — do not use it.
- The redirect URI must be registered under the **"Mobile and desktop applications"**
  platform in Azure Portal (not "Web"), otherwise Azure demands a `client_secret`.
- `oauth2.AuthStyleInParams` must be set explicitly on the endpoint; without it
  the library probes with `Authorization: Basic`, confusing Azure into treating
  the app as a confidential client.

**GitHub Copilot token exchange:**
- `POST api.github.com/copilot_internal/v2/token` rejects PATs with 403. It
  requires an OAuth token obtained via `gh auth login --scopes copilot`.

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

<!-- BEGIN BEADS INTEGRATION -->
## Issue Tracking with bd (beads)

**IMPORTANT**: This project uses **bd (beads)** for ALL issue tracking. Do NOT use markdown TODOs, task lists, or other tracking methods.

### Why bd?

- Dependency-aware: Track blockers and relationships between issues
- Git-friendly: Dolt-powered version control with native sync
- Agent-optimized: JSON output, ready work detection, discovered-from links
- Prevents duplicate tracking systems and confusion

### Quick Start

**Check for ready work:**

```bash
bd ready --json
```

**Create new issues:**

```bash
bd create "Issue title" --description="Detailed context" -t bug|feature|task -p 0-4 --json
bd create "Issue title" --description="What this issue is about" -p 1 --deps discovered-from:bd-123 --json
```

**Claim and update:**

```bash
bd update <id> --claim --json
bd update bd-42 --priority 1 --json
```

**Complete work:**

```bash
bd close bd-42 --reason "Completed" --json
```

### Issue Types

- `bug` - Something broken
- `feature` - New functionality
- `task` - Work item (tests, docs, refactoring)
- `epic` - Large feature with subtasks
- `chore` - Maintenance (dependencies, tooling)

### Priorities

- `0` - Critical (security, data loss, broken builds)
- `1` - High (major features, important bugs)
- `2` - Medium (default, nice-to-have)
- `3` - Low (polish, optimization)
- `4` - Backlog (future ideas)

### Workflow for AI Agents

1. **Check ready work**: `bd ready` shows unblocked issues
2. **Claim your task atomically**: `bd update <id> --claim`
3. **Work on it**: Implement, test, document
4. **Discover new work?** Create linked issue:
   - `bd create "Found bug" --description="Details about what was found" -p 1 --deps discovered-from:<parent-id>`
5. **Complete**: `bd close <id> --reason "Done"`

### Auto-Sync

bd automatically syncs via Dolt:

- Each write auto-commits to Dolt history
- Use `bd dolt push`/`bd dolt pull` for remote sync
- No manual export/import needed!

### Important Rules

- ✅ Use bd for ALL task tracking
- ✅ Always use `--json` flag for programmatic use
- ✅ Link discovered work with `discovered-from` dependencies
- ✅ Check `bd ready` before asking "what should I work on?"
- ❌ Do NOT create markdown TODO lists
- ❌ Do NOT use external issue trackers
- ❌ Do NOT duplicate tracking systems

For more details, see README.md and docs/QUICKSTART.md.

## Landing the Plane (Session Completion)

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd sync
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds

<!-- END BEADS INTEGRATION -->
