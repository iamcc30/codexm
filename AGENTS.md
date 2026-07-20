# Repository Guidelines

## Project Structure & Module Organization

`codexm` is a Go 1.22 command-line application. The executable entry point is
`cmd/codexm/main.go`. Core packages live under `internal/`: `cli` parses commands,
`config` manages profiles and project bindings, `codex` invokes the upstream
Codex CLI, and `sharedmcp` synchronizes MCP configuration. Tests sit beside their
packages as `*_test.go`. Shell and PowerShell helpers are in `scripts/`;
cross-platform release automation lives in `.github/workflows/release.yml`.
User-facing documentation is maintained in both `README.md` and
`README.zh-CN.md`. Generated binaries and archives belong in `bin/` or `dist/`
and must not be committed.

## Build, Test, and Development Commands

- `go build -o codexm ./cmd/codexm` builds a local executable.
- `go test ./...` runs all package tests.
- `go vet ./...` performs standard Go static analysis.
- `go fmt ./...` formats all Go sources.
- `./scripts/build-all.sh dev` tests and creates archives for supported Linux,
  macOS, and Windows targets under `dist/`.

Run the formatter, tests, and vet before opening a pull request. The external
`codex` executable is required for manual end-to-end CLI checks.

## Coding Style & Naming Conventions

Follow idiomatic Go and let `gofmt` determine indentation and layout. Use short,
lowercase package names; exported identifiers use `PascalCase`, unexported
identifiers use `camelCase`. Keep command handlers consistent with the existing
`cmdXxx` methods in `internal/cli`. Return errors with useful context, preserve
wrapped causes where appropriate, and avoid exposing credentials or profile
secrets in output.

## Testing Guidelines

Use Go's standard `testing` package. Name tests `TestBehaviorOrScenario` and keep
fixtures isolated with `t.TempDir()`. Add regression tests beside the changed
package for bug fixes and cover both success and failure paths. There is no
numeric coverage target; prioritize profile isolation, path resolution,
configuration preservation, and idempotent synchronization behavior.

## Commit & Pull Request Guidelines

Recent history uses concise Conventional Commit-style subjects such as
`feat: share MCP configuration across profiles`, `docs: ...`, and `ci: ...`.
Keep each commit focused and written in the imperative mood. Pull requests
should explain the motivation and behavior change, link relevant issues, and
list verification commands. Include terminal output or screenshots when CLI
output changes. Update `CHANGELOG.md` plus both README translations for
user-visible changes.

## Security & Configuration

Never commit authentication files, API keys, or real profile data. Tests should
use temporary homes and synthetic TOML. Changes involving `CODEX_HOME`,
credential stores, or MCP OAuth must preserve isolation between profiles.
