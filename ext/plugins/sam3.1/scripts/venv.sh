#!/usr/bin/env bash
# Create a venv (Python >= 3.12) with the shim. No sudo, nothing outside the venv.
# Dev default: ./.venv, editable, with test deps.
# Plugin install: VENV=<dir> SHIM_SPEC=. (non-editable, no test deps).
set -euo pipefail
cd "$(dirname "$0")/.."
VENV="${VENV:-.venv}"
SHIM_SPEC="${SHIM_SPEC:--e .[test]}"
uv venv --python ">=3.12" "$VENV"
# shellcheck disable=SC2086
uv pip install --python "$VENV/bin/python" $SHIM_SPEC
