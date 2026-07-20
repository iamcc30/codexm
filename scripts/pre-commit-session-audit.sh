#!/usr/bin/env sh
set -eu

if [ ! -f .codexm/project.json ]; then
  exit 0
fi

codexm session audit --strict
