# Changelog

## Unreleased

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
