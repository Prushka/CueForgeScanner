#!/usr/bin/env bash
set -euo pipefail

export CUEFORGE_SCAN_DIR="/Users/dan/GolandProjects/Sparkle/output"
export CUEFORGE_BASE_URL="http://localhost:8080"
export CUEFORGE_INPUT_LANGUAGES="eng,ger,chi"
export CUEFORGE_TARGET_LANGUAGES='chi,$jpn'
export CUEFORGE_MODEL="gpt-5.4-mini"
export CUEFORGE_VMODEL="gpt-5.4-mini"
export CUEFORGE_REASONING_EFFORT="medium"
export CUEFORGE_MEDIA=""
export CUEFORGE_REQUEST_TIMEOUT="30m"

go run ./cmd/scanner
