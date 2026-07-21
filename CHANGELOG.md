# Changelog

## Unreleased

## 0.3.1 - 2026-07-21

- Fixed `codexm run` launching managed remote Codex sessions with the app-server
  daemon's directory instead of the selected project working directory.

## 0.3.0 - 2026-07-20

- Added `codexm session audit` with JSON/strict modes, secret-safe findings,
  common credential and high-entropy detection, transcript size checks,
  portable-cwd validation, Zstandard support, and a pre-commit hook example.
- Hardened profile storage against dot-segment directory escape and serialized
  configuration reads and updates across processes with locked temporary-file
  transactions.
- Reduced session synchronization work by filtering unrelated histories from
  their first metadata record, reusing preflight scans, and using lightweight
  before/after snapshots.
- Added bounded monitor caches for process, Git, project-attribution, and mirror
  inspection data so live app-server notifications do not trigger repeated cold
  filesystem and process-table scans.
- Added pull-request CI with formatting, module verification, vet, race tests,
  fuzz targets, and Linux, macOS, and Windows test/build jobs.
- Added a read-only multi-profile monitoring layer with responsive Web Dashboard,
  authenticated SSE/JSON endpoints, server-side session pagination and filters,
  terminal TUI, project/session/task views, per-server MCP health, account usage
  and rate limits, cross-platform unmanaged-process discovery, and cycle-safe
  subagent trees.
- Added profile-scoped managed Codex app-server daemons, authenticated loopback
  WebSockets, capability-aware `run` remote injection, `--unmanaged`, protected
  daemon shutdown, overload backoff, crash recovery, private runtime state, and
  diagnostic checks.
- Added explicit LAN Dashboard mode with self-signed HTTPS SAN certificates,
  high-entropy access tokens, secure cookies, and token rotation.
- Added opt-in project-level Codex session mirrors with bidirectional `run`
  synchronization, explicit import, status, conflict resolution, archive/name
  round trips, tombstone deletion propagation, portable cwd mapping, JSONL and
  Zstandard support, and private local conflict backups.
- Added Homebrew installation through `iamcc30/tap/codexm`.
- Added a one-command release helper that publishes GitHub Releases and updates Homebrew.
- Added step-by-step Windows installation instructions in English and Simplified Chinese.

## 0.2.0 - 2026-07-14

- Added shared MCP server management through `codexm mcp` commands.
- Added automatic MCP synchronization with per-profile exclusions and local override precedence.
- Added file-based isolation for MCP OAuth credentials by default.
- Documented user-level and repository-level skill reuse.
- Added English and Simplified Chinese project documentation.
- Changed the Go module path to `github.com/iamcc30/codexm` for public installation.
- Extended diagnostics for shared MCP drift and MCP OAuth credential storage.

## 0.1.0 - 2026-07-14

- Initial cross-platform release.
- Isolated Codex account profiles through independent `CODEX_HOME` directories.
- File-based credential isolation by default.
- Project-to-profile bindings with nearest-parent resolution.
- Login, logout, status, run, shell, default profile, removal, and diagnostics.
- macOS, Linux, and Windows build automation.
