# codexm

English | [简体中文](README.zh-CN.md)

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.22+](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev/)

`codexm` is an OpenAI Codex CLI multi-account manager, account switcher,
profile manager, and multi-project manager.

It assigns an isolated `CODEX_HOME` to each account, keeping authentication, `config.toml`, session history, logs, and caches separate. Project bindings then select the correct account automatically based on your working directory.

> `codexm` is an independent open-source project and is not an official OpenAI product. It never asks for your ChatGPT password and delegates authentication to the official `codex login` flow.

## Features

- Isolated Codex profiles: one `CODEX_HOME` per account
- File-based isolation for ChatGPT credentials and MCP OAuth credentials by default
- Shared MCP server definitions across profiles, with per-profile exclusions
- Independent MCP OAuth login per profile
- Native support for user-level and repository-level Codex Skills
- Project-to-profile bindings with nearest-parent directory resolution
- Opt-in, repository-backed project session mirrors for multi-device and team handoff
- Read-only browser Dashboard and terminal TUI across every profile and project
- On-demand, profile-isolated Codex app-server management for live task status
- Automatic profile selection or explicit selection with `codexm run PROFILE`
- Full passthrough of Codex CLI arguments
- Login, logout, status, shell, diagnostics, and profile lifecycle commands
- Native macOS, Linux, and Windows support
- No token, password, or API key storage in the `codexm` manager configuration

## How it works

Codex stores local state under `~/.codex` by default. `codexm` creates a separate directory for every account and launches Codex with:

```text
CODEX_HOME=<isolated profile directory>
```

For example:

```text
account1 -> ~/.local/share/codexm/profiles/account1
account2 -> ~/.local/share/codexm/profiles/account2
```

Default paths follow the conventions of each operating system and can be overridden with environment variables.

## Requirements

1. Install the OpenAI Codex CLI and make sure it is available in `PATH`:

```bash
codex --version
```

2. Use a prebuilt `codexm` binary, or install Go 1.22+ to build from source.

## Quick start

### 1. Add account profiles

```bash
codexm add --description "Project 1 account" account1
codexm add --description "Project 2 account" account2
```

The first profile becomes the default. You can change it at any time:

```bash
codexm default account1
```

### 2. Sign in to each account

```bash
codexm login account1
codexm login account2
```

When the browser opens, make sure you authorize the intended ChatGPT account.

For remote servers or environments where the browser callback is unavailable:

```bash
codexm login --device account1
```

### 3. Bind projects

macOS and Linux:

```bash
codexm bind account1 ~/Projects/project1
codexm bind account2 ~/Projects/project2
```

Windows PowerShell:

```powershell
codexm bind account1 D:\Projects\project1
codexm bind account2 D:\Projects\project2
```

### 4. Run Codex in a project

```bash
cd ~/Projects/project1
codexm run
```

`codexm` recognizes that the directory belongs to `project1` and launches Codex with `account1`.

From another terminal:

```bash
cd ~/Projects/project2
codexm run
```

This automatically selects `account2`.

### 5. Add an MCP server shared by all profiles

STDIO MCP server:

```bash
codexm mcp add context7 -- npx -y @upstash/context7-mcp
```

Remote MCP server:

```bash
codexm mcp add docs \
  --url https://example.com/mcp \
  --bearer-token-env-var DOCS_TOKEN
```

`codexm` uses the official `codex mcp` commands to maintain the shared definitions and synchronizes them to existing profiles. New profiles inherit them automatically.

## Common commands

### Inspect profiles

```bash
codexm list
codexm list --status
codexm show account1
```

### Show the profile selected for a directory

```bash
codexm current
```

Example output:

```text
account1    /Users/you/Projects/project1
```

### Select a profile explicitly

```bash
codexm run account1
codexm run account2
```

When no profile is specified, selection follows this priority:

1. The closest project binding for the current directory or one of its parents
2. The default profile
3. An error when neither exists

### Pass arguments to Codex

Everything after `--` is forwarded to Codex unchanged:

```bash
codexm run account1 -- --model gpt-5.6
codexm run account1 -- exec "review this project's code quality"
codexm run -- resume --all
```

### Open a profile shell

```bash
codexm shell account1
```

The new shell has `CODEX_HOME` set for `account1`, so you can run Codex directly:

```bash
codex
codex login status
codex resume --all
```

Exiting the child shell leaves the parent shell unchanged.

`codexm shell` intentionally bypasses automatic project session synchronization.
Run `codexm session sync` after using Codex from that shell when the project has
session mirroring enabled.

### Check login status

```bash
codexm status account1
codexm status --all
```

## Read-only monitoring

Open the terminal UI or browser Dashboard. Both views aggregate all profiles by
default and can be narrowed to one profile or project:

```bash
codexm ui
codexm ui --profile account1 --project ~/Projects/project1

codexm dashboard
codexm dashboard --profile account1 --project ~/Projects/project1
```

The shared monitor shows account plan and rate-limit windows, token-usage
summaries, project bindings and Git metadata, portable-session mirror health,
session metadata, active/waiting/idle/error tasks, and the subagent hierarchy.
It follows the system locale for English or Simplified Chinese and refreshes
from the official Codex App Server protocol.

The Dashboard session view uses server-side search, sorting, filtering, and
pagination, including profile, project, state, archive, source, and model
filters. Account cards report MCP authentication/startup health per configured
server. Rate limits remain profile-scoped: profiles logged into the same
ChatGPT account are shown separately and their quotas are never added together.

Account-service usage and locally observed per-thread token totals are separate
measurements and are not billing amounts. The App Server does not currently
expose historical per-thread model or token totals through `thread/list`;
codexm displays those fields as unavailable until a live settings/reroute or
token-usage notification supplies them. It does not substitute zero.

Monitoring is strictly read-only. The TUI, JSON API, and SSE stream cannot start,
interrupt, approve, archive, or delete work. They cache only metadata and a
short first-prompt preview; full transcripts, tool output, and command output
are never loaded into the monitoring model or exposed by the Dashboard API.

The Dashboard listens on a random `127.0.0.1` port by default, generates a
private access token, and opens a tokenized URL that establishes an HttpOnly
session cookie. To use it from another LAN device:

```bash
codexm dashboard --lan --listen 0.0.0.0:7443 --no-open
codexm dashboard --lan --listen 0.0.0.0:7443 --rotate-token
```

LAN mode is explicit and uses a generated self-signed HTTPS certificate with
local-address SANs plus a high-entropy access token. Trust the certificate on
each client device. The underlying Codex app-server endpoints always remain on
loopback and are never exposed to the LAN.

### Managed app-server daemons

Interactive `codexm run` invocations start a profile-specific app-server on
demand and connect Codex through its authenticated loopback WebSocket endpoint.
The daemon survives Dashboard/TUI exit so other terminals can keep using it:

```bash
codexm daemon start account1
codexm daemon start --all
codexm daemon status --all
codexm daemon stop account1
codexm daemon stop --all --force
```

A normal stop refuses while the profile has an active thread; `--force` is an
explicit override. Runtime state, capability tokens, certificates, and private
logs live under the codexm manager's `runtime/` directory with private
permissions. They are never written into a profile or project repository.

Managed remote mode applies to interactive Codex, `resume`, `fork`, `archive`,
`delete`, and `unarchive`. Commands whose CLI surface does not support
`--remote`, including `exec` and `review`, continue directly and appear as
unmanaged tasks. Use this explicit escape hatch to retain the old behavior:

```bash
codexm run --unmanaged account1 -- resume --last
```

A custom child `--remote` is only accepted together with `--unmanaged`.
`codexm shell` and Codex processes started outside codexm are also unmanaged.
Older Codex versions that do not advertise app-server remote capabilities fall
back to direct execution. If capabilities are present but the managed service
fails to start, the run stops with an error instead of silently changing its
execution model.

Codex app-server WebSocket and remote TUI support are currently experimental.
codexm uses capability detection and tolerates unavailable optional methods,
unknown response fields, bounded-server overloads, reconnects, and one-profile
failures, but a future Codex release may still require a compatibility update.
See the official
[Codex App Server documentation](https://learn.chatgpt.com/docs/app-server.md)
and [Codex CLI command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).

## Reusing MCP servers and Skills

### Shared MCP servers

Inspect shared servers:

```bash
codexm mcp list
codexm mcp get context7
```

Adding or removing a shared server automatically synchronizes all profiles. You can also synchronize manually:

```bash
codexm mcp sync --all
codexm mcp sync account1
```

Exclude a shared server from one profile, or include it again:

```bash
codexm mcp exclude account2 production-db
codexm mcp include account2 production-db
```

Server definitions are shared; OAuth credentials are not. Authenticate OAuth-backed MCP servers separately for each profile:

```bash
codexm mcp login account1 github --scopes repo
codexm mcp login account2 github --scopes repo
codexm mcp logout account2 github
```

Shared MCP configuration is stored in an isolated shared `CODEX_HOME`. Print its path with:

```bash
codexm mcp path
```

Synchronization only updates the marked `codexm` block in each profile's `config.toml`:

- Models, sandbox settings, features, comments, and other profile configuration are preserved.
- A profile-local `[mcp_servers.NAME]` definition overrides a shared server with the same name.
- Per-profile exclusions are stored in the `codexm` manager configuration.
- `run` and `shell` repair synchronization drift before starting Codex.

Prefer `bearer_token_env_var`, `env_vars`, or `env_http_headers` references for secrets. Do not write real tokens into shared MCP configuration. In particular, `codex mcp add --env KEY=VALUE` persists that value as configuration and `codexm` will synchronize it.

### Shared Skills

Put personal Skills in Codex's user-level Skills directory:

```text
$HOME/.agents/skills/<skill-name>/SKILL.md
```

`codexm` does not change `HOME`, so every profile discovers these Skills without copying or synchronization.

Put project-specific Skills in the repository:

```text
<repo>/.agents/skills/<skill-name>/SKILL.md
```

Project-specific MCP servers should usually be configured in a trusted repository's:

```text
<repo>/.codex/config.toml
```

Do not symlink profile `CODEX_HOME/skills/.system` directories or entire `config.toml` files. System Skills are managed by Codex, while `config.toml` also contains profile-specific runtime settings.

## Portable project sessions

Project session mirroring is disabled by default. Enable it explicitly from a
project root:

```bash
codexm session init
```

This creates a stable, account-independent project ID and a `.codexm/` mirror.
It starts empty. To import existing active and archived sessions whose initial
working directory is inside the project, opt in explicitly:

```bash
codexm session init --import-existing
```

After initialization, `codexm run` imports project changes before starting
Codex and exports changes again after Codex exits, including after a nonzero
exit or interruption. New sessions, continued conversations, names, archive
state, restores, and deletions round-trip. A different checkout can bind its own
local profile and continue normally:

```bash
git clone <private-repository> project
cd project
codexm bind my-local-profile .
codexm run -- resume
```

The nearest parent `.codexm/project.json` is used, so nested projects in a
monorepo are supported. `--profile` overrides the local profile for one session
command and is never written to the repository.

Inspect or synchronize without starting Codex:

```bash
codexm session status
codexm session sync
```

Audit the mirror before committing `.codexm/`:

```bash
codexm session audit
codexm session audit --strict
codexm session audit --json
```

The audit checks common credential formats, high-entropy values, oversized
transcripts, damaged JSONL, and absolute structured `cwd` values without a
sidecar mapping. It reports only locations and finding types, never matched
secret values. Errors return a non-zero status by default; `--strict` also
blocks warnings for pre-commit use. A ready-to-use example is provided at
`scripts/pre-commit-session-audit.sh`.

When one copy is a strict JSONL prefix of the other, the longer copy wins. If
both copies independently appended to the same session, synchronization stops
before Codex starts. Choose a winner explicitly:

```bash
codexm session resolve --use project SESSION_ID
codexm session resolve --use profile SESSION_ID
```

The losing copy is backed up under the private `CODEXM_HOME/session-backups/`
directory first. For concurrent team work, use Codex `fork` so each branch gets
a different session ID.

The repository mirror contains only replayable session data:

```text
.codexm/
├── project.json
├── .gitattributes
├── sessions/YYYY/MM/DD/rollout-*.jsonl[.zst]
├── archived_sessions/rollout-*.jsonl[.zst]
├── metadata/<session-id>.json
└── tombstones/<session-id>.json
```

It never copies `auth.json`, credentials, `config.toml`, logs, history, or
SQLite state. Structured `session_meta.cwd` and `turn_context.cwd` values are
mapped to the current checkout when importing; other transcript content is
preserved. The managed `.gitattributes` block prevents Git from silently
combining two versions of the same session.

> Session files are unencrypted and may contain prompts, command output,
> absolute paths, source snippets, or secrets. Prefer a private repository and
> review `.codexm/` before every commit. `codexm` never runs `git add`, commits,
> or pushes these files.

## Profile management

### Unbind a project

```bash
codexm unbind ~/Projects/project1
```

From the exact project root, this can be shortened to:

```bash
codexm unbind
```

### Remove a profile

Remove the manager record but keep its `CODEX_HOME`:

```bash
codexm remove account1
```

Delete both the record and profile directory:

```bash
codexm remove --delete-home --yes account1
```

This operation cannot be undone.

### Diagnose the installation

```bash
codexm doctor
```

The doctor checks:

- Whether the Codex CLI is available in `PATH`
- Whether the manager configuration can be loaded
- Whether every profile `CODEX_HOME` exists
- Whether ChatGPT and MCP OAuth credentials use isolated file storage
- Whether shared MCP configuration is valid and synchronized
- Whether project bindings reference valid profiles

## Adopt an existing CODEX_HOME

Existing manually-created Codex homes can be registered directly:

```bash
codexm add --home ~/.codex-account1 account1
codexm add --home ~/.codex-account2 account2
```

`codexm` preserves the existing `config.toml` and adds or updates:

```toml
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "file"
```

Profiles created by older `codexm` versions that do not yet specify MCP OAuth storage are migrated on the next `run`, `shell`, or MCP synchronization. The migration follows the existing ChatGPT credential-store choice and does not override an explicit MCP OAuth setting.

If credentials were previously stored in the system keychain, you may need to sign in again after switching to file storage.

## Clone non-sensitive configuration

To reuse model, sandbox, or feature configuration when creating a profile:

```bash
codexm add --clone-config account1 account2
```

Only `config.toml` is copied. `auth.json` and other credentials are never cloned. Sign in separately afterward:

```bash
codexm login account2
```

## Installation

### Homebrew (macOS and Linux)

```bash
brew install iamcc30/tap/codexm
```

Upgrade later with:

```bash
brew update
brew upgrade iamcc30/tap/codexm
```

### Prebuilt binaries

Choose the archive for your platform from [GitHub Releases](https://github.com/iamcc30/codexm/releases):

- macOS Apple Silicon: `darwin-arm64`
- macOS Intel: `darwin-amd64`
- Linux x86_64: `linux-amd64`
- Linux ARM64: `linux-arm64`
- Windows x86_64: `windows-amd64`
- Windows ARM64: `windows-arm64`

Extract the archive and place `codexm` or `codexm.exe` somewhere in `PATH`.

### Windows installation

1. Open [GitHub Releases](https://github.com/iamcc30/codexm/releases/latest)
   and download the archive ending in `_windows_amd64.zip` for a typical
   Intel/AMD Windows PC, or `_windows_arm64.zip` for Windows on ARM.
2. Extract the ZIP file, open the extracted directory that contains
   `codexm.exe`, and start PowerShell there.
3. Run the following commands to copy the executable into your user profile and
   add it to your user `PATH`:

```powershell
$installDir = Join-Path $env:LOCALAPPDATA "Programs\codexm"
New-Item -ItemType Directory -Force -Path $installDir | Out-Null
Copy-Item .\codexm.exe "$installDir\codexm.exe" -Force
Unblock-File "$installDir\codexm.exe"

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$pathEntries = @($userPath -split ";" | Where-Object { $_ })
if ($pathEntries -notcontains $installDir) {
    [Environment]::SetEnvironmentVariable(
        "Path",
        (($pathEntries + $installDir) -join ";"),
        "User"
    )
}
$env:Path = "$env:Path;$installDir"
```

Verify the installation:

```powershell
codexm version
codex --version
```

The second command confirms that the OpenAI Codex CLI dependency is also
available. To upgrade `codexm`, download the newer ZIP and repeat the copy
command to replace `%LOCALAPPDATA%\Programs\codexm\codexm.exe`.

### Build from source

```bash
git clone https://github.com/iamcc30/codexm.git
cd codexm
go test ./...
go build -o codexm ./cmd/codexm
```

macOS and Linux:

```bash
./scripts/install.sh
```

Windows PowerShell:

```powershell
.\scripts\install.ps1
```

### Install with Go

Install the latest published version directly:

```bash
go install github.com/iamcc30/codexm/cmd/codexm@latest
```

To install from a local checkout instead:

```bash
go install ./cmd/codexm
```

Make sure your Go binary directory is in `PATH`.

## Environment variables

| Variable | Purpose |
|---|---|
| `CODEXM_HOME` | Override the `codexm` manager configuration directory |
| `CODEXM_PROFILES_HOME` | Override the default root for new profile `CODEX_HOME` directories |
| `CODEXM_CODEX_BIN` | Override the Codex CLI executable or command name |

Example:

```bash
CODEXM_PROFILES_HOME=/data/codex-profiles codexm add account1
```

## Default storage locations

### macOS

```text
Manager config: ~/Library/Application Support/codexm/config.json
Shared MCP:    ~/Library/Application Support/codexm/shared/config.toml
Profiles:      ~/Library/Application Support/codexm/profiles/<name>
```

### Linux

```text
Manager config: ~/.config/codexm/config.json
Shared MCP:    ~/.config/codexm/shared/config.toml
Profiles:      ~/.local/share/codexm/profiles/<name>
```

`XDG_CONFIG_HOME` and `XDG_DATA_HOME` are supported.

### Windows

```text
Manager config: %APPDATA%\codexm\config.json
Shared MCP:    %APPDATA%\codexm\shared\config.toml
Profiles:      %LOCALAPPDATA%\codexm\profiles\<name>
```

## Security

- `config.json` contains profile metadata, project bindings, and shared MCP exclusions, but no login credentials.
- With file storage, official Codex credentials live in each profile's `auth.json`.
- MCP OAuth credentials also use profile-local file storage by default; synchronization never copies them.
- Treat `auth.json` like a password. Never commit, upload, or share it.
- On macOS and Linux, profile directories are created with `0700` permissions and configuration files with `0600` permissions.
- Avoid `keyring` for strict multi-account isolation because system credential storage can bypass the `CODEX_HOME` directory boundary.
- Run `codexm logout PROFILE` before deleting a profile when possible.
- Repository `.codexm/` session mirrors are unencrypted transcripts, not
  credentials, but may still contain sensitive content. Review them before
  committing and prefer private repositories.

## Command reference

```text
codexm init
codexm add [--home PATH] [--description TEXT] [--bind PATH]
           [--credential-store file|auto|keyring]
           [--clone-config PROFILE] NAME
codexm remove [--delete-home --yes] NAME
codexm list [--status]
codexm show NAME
codexm default [NAME|--clear]
codexm bind PROFILE [PATH]
codexm unbind [PATH]
codexm current [PATH]
codexm login [--device] PROFILE
codexm logout PROFILE
codexm status [PROFILE|--all]
codexm mcp add [CODEX_MCP_ADD_ARGS...]
codexm mcp remove NAME
codexm mcp list
codexm mcp get NAME
codexm mcp sync [PROFILE|--all]
codexm mcp exclude PROFILE SERVER
codexm mcp include PROFILE SERVER
codexm mcp login PROFILE NAME [CODEX_MCP_LOGIN_ARGS...]
codexm mcp logout PROFILE NAME
codexm mcp path
codexm session init [--project PATH] [--profile PROFILE] [--import-existing]
codexm session sync [--project PATH] [--profile PROFILE]
codexm session status [--project PATH] [--profile PROFILE]
codexm session audit [--project PATH] [--strict] [--json]
                     [--max-file-size-mb N]
codexm session resolve [--project PATH] [--profile PROFILE]
                       --use project|profile SESSION_ID
codexm run [--project PATH] [PROFILE] -- [CODEX_ARGS...]
codexm shell PROFILE
codexm doctor
codexm config-path
codexm version
```

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/build-all.sh 0.1.0
```

Cross-platform release archives are written to `dist/`.

## Releasing

The release workflow supports both manual dispatch and pushed semantic-version tags. Before publishing, move the pending entries under `Unreleased` in [CHANGELOG.md](CHANGELOG.md) into a new version-and-date section, keep an empty `Unreleased` section at the top, then commit and push it.

The quickest way to publish both a GitHub Release and the matching Homebrew
formula is:

```bash
./scripts/release.sh 0.2.1
```

The helper verifies that `main` is clean and pushed, waits for the release to
finish, and then updates `iamcc30/homebrew-tap`. Prerelease versions are not
published to Homebrew.

To publish only the GitHub Release, run:

```bash
gh workflow run release.yml -f version=0.2.1
gh run watch
```

The Tap also checks for the latest stable release once a day. To update it
manually, run:

```bash
gh workflow run update.yml --repo iamcc30/homebrew-tap -f version=0.2.1
```

You can also open **Actions → release → Run workflow** on GitHub and enter `0.2.1`. The workflow validates the version and changelog, runs the tests, builds all supported platforms, creates tag `v0.2.1`, and publishes the GitHub Release with checksums.

Traditional tag-driven releases remain supported:

```bash
git tag -a v0.2.1 -m "codexm v0.2.1"
git push origin v0.2.1
```

## Contributing

Issues and pull requests are welcome. Before submitting code, run:

```bash
go fmt ./...
go test ./...
go vet ./...
```

Keep changes focused and add tests for new behavior or bug fixes. Update [CHANGELOG.md](CHANGELOG.md) and both README files when a change affects users. Please use [GitHub Issues](https://github.com/iamcc30/codexm/issues) for bug reports and feature requests.

## Design references

- Codex stores local state in `CODEX_HOME`, which defaults to `~/.codex`.
- `cli_auth_credentials_store = "file"` stores credentials in the active `CODEX_HOME/auth.json`.
- `mcp_oauth_credentials_store = "file"` keeps MCP OAuth credentials inside the profile boundary.
- User Skills live under `$HOME/.agents/skills`; repository Skills live under `.agents/skills`.
- MCP configuration can live in user `config.toml` or a trusted project's `.codex/config.toml`.
- Session transcripts live under `CODEX_HOME/sessions`; archived transcripts
  live under `CODEX_HOME/archived_sessions`. SQLite-backed metadata can be
  rebuilt from rollout files.
- Authentication commands are delegated to the official Codex CLI.

Official documentation:

- https://learn.chatgpt.com/docs/auth
- https://learn.chatgpt.com/docs/config-file/config-advanced
- https://learn.chatgpt.com/docs/developer-commands?surface=cli
- https://learn.chatgpt.com/docs/build-skills
- https://learn.chatgpt.com/docs/extend/mcp
- https://github.com/openai/codex/blob/rust-v0.144.5/codex-rs/thread-store/src/local/mod.rs
- https://github.com/openai/codex/blob/rust-v0.144.5/codex-rs/rollout/src/metadata.rs

## License

This project is licensed under the [MIT License](LICENSE).
