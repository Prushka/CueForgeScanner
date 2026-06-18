#!/usr/bin/env bash
set -euo pipefail

export CUEFORGE_SCAN_DIR="/Users/dan/GolandProjects/Sparkle/output"
export CUEFORGE_BASE_URL="http://localhost:8081"
export CUEFORGE_INPUT_LANGUAGES="eng,chi,rus"
export CUEFORGE_TARGET_LANGUAGES='$chi,$jpn,$rus,$spa,$fra'
export CUEFORGE_REASONING_EFFORT="high"
export CUEFORGE_REQUEST_TIMEOUT="30m"
export CUEFORGE_CONCURRENCY="2"

go run ./cmd/scanner
