# codexm

English | [简体中文](README.zh-CN.md)

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.22+](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev/)

`codexm` is a multi-account and multi-project manager for the OpenAI Codex CLI.

It assigns an isolated `CODEX_HOME` to each account, keeping authentication, `config.toml`, session history, logs, and caches separate. Project bindings then select the correct account automatically based on your working directory.

> `codexm` is an independent open-source project and is not an official OpenAI product. It never asks for your ChatGPT password and delegates authentication to the official `codex login` flow.

## Features

- Isolated Codex profiles: one `CODEX_HOME` per account
- File-based isolation for ChatGPT credentials and MCP OAuth credentials by default
- Shared MCP server definitions across profiles, with per-profile exclusions
- Independent MCP OAuth login per profile
- Native support for user-level and repository-level Codex Skills
- Project-to-profile bindings with nearest-parent directory resolution
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

### Check login status

```bash
codexm status account1
codexm status --all
```

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

### Prebuilt binaries

Choose the archive for your platform from [GitHub Releases](https://github.com/iamcc30/codexm/releases):

- macOS Apple Silicon: `darwin-arm64`
- macOS Intel: `darwin-amd64`
- Linux x86_64: `linux-amd64`
- Linux ARM64: `linux-arm64`
- Windows x86_64: `windows-amd64`
- Windows ARM64: `windows-arm64`

Extract the archive and place `codexm` or `codexm.exe` somewhere in `PATH`.

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
codexm run [--project PATH] [PROFILE] -- [CODEX_ARGS...]
codexm shell PROFILE
codexm doctor
codexm config-path
codexm version
```

## Development

```bash
go test ./...
./scripts/build-all.sh 0.1.0
```

Cross-platform release archives are written to `dist/`.

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
- Authentication commands are delegated to the official Codex CLI.

Official documentation:

- https://learn.chatgpt.com/docs/auth
- https://learn.chatgpt.com/docs/config-file/config-advanced
- https://learn.chatgpt.com/docs/developer-commands?surface=cli
- https://learn.chatgpt.com/docs/build-skills
- https://learn.chatgpt.com/docs/extend/mcp

## License

This project is licensed under the [MIT License](LICENSE).
