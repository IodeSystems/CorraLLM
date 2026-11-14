#!/usr/bin/env bash
# Create ./.venv (Python >= 3.12) with test deps. No sudo, nothing outside the repo.
set -euo pipefail
cd "$(dirname "$0")/.."
uv venv --python ">=3.12" .venv
uv pip install --python .venv/bin/python -e ".[test]"
