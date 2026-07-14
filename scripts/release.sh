#!/usr/bin/env bash
set -euo pipefail

repo="iamcc30/codexm"
tap_repo="iamcc30/homebrew-tap"
version="${1:-}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "Usage: $0 VERSION (for example, 0.2.1)" >&2
  exit 1
fi

for command in gh git; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required command not found: $command" >&2
    exit 1
  fi
done

if [[ -n "$(git status --porcelain)" ]]; then
  echo "The working tree must be clean before publishing." >&2
  exit 1
fi

if [[ "$(git branch --show-current)" != "main" ]]; then
  echo "Run this command from the main branch." >&2
  exit 1
fi

git fetch --quiet origin main
if [[ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]]; then
  echo "Local main and origin/main must point to the same commit." >&2
  exit 1
fi

if ! grep -Fq "## ${version} - " CHANGELOG.md; then
  echo "CHANGELOG.md is missing a release heading for ${version}." >&2
  exit 1
fi

latest_run_id() {
  local target_repo=$1
  local workflow=$2
  gh run list \
    --repo "$target_repo" \
    --workflow "$workflow" \
    --event workflow_dispatch \
    --limit 1 \
    --json databaseId \
    --jq '.[0].databaseId // empty'
}

wait_for_new_run() {
  local target_repo=$1
  local workflow=$2
  local previous_id=$3
  local run_id

  for _ in {1..30}; do
    run_id="$(latest_run_id "$target_repo" "$workflow")"
    if [[ -n "$run_id" && "$run_id" != "$previous_id" ]]; then
      printf '%s' "$run_id"
      return 0
    fi
    sleep 2
  done

  echo "Timed out waiting for ${target_repo}/${workflow} to start." >&2
  return 1
}

previous_release_run="$(latest_run_id "$repo" release.yml)"
echo "Starting codexm v${version} release..."
gh workflow run release.yml --repo "$repo" -f "version=${version}"
release_run="$(wait_for_new_run "$repo" release.yml "$previous_release_run")"
gh run watch "$release_run" --repo "$repo" --exit-status

if [[ "$version" == *-* ]]; then
  echo "Published prerelease v${version}; Homebrew remains on the latest stable version."
  exit 0
fi

previous_tap_run="$(latest_run_id "$tap_repo" update.yml)"
echo "Updating the Homebrew formula to ${version}..."
gh workflow run update.yml --repo "$tap_repo" -f "version=${version}"
tap_run="$(wait_for_new_run "$tap_repo" update.yml "$previous_tap_run")"
gh run watch "$tap_run" --repo "$tap_repo" --exit-status

echo "Published codexm v${version} and updated Homebrew."
