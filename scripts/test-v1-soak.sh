#!/usr/bin/env bash
set -euo pipefail

report_dir=.state/v1-soak
mkdir -p "$report_dir"
export REGATE_SOAK_DURATION="${REGATE_SOAK_DURATION:-6m}"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
export REGATE_SOAK_GATEWAY_BINARY="$scratch/regate"

run() {
    local commit changes
    commit=$(git rev-parse HEAD) || return
    changes=$(git status --porcelain) || return
    printf 'commit=%s\n' "$commit"
    if [[ -n "$changes" ]]; then
        printf 'worktree=dirty\n'
    else
        printf 'worktree=clean\n'
    fi
    go build -race -o "$REGATE_SOAK_GATEWAY_BINARY" ./cmd/regate || return
    go test -race ./internal/server -run '^TestV1Soak$' -count=1 -timeout 20m -v
}

printf 'Mixed-load report: %s/latest.log\n' "$report_dir"
run 2>&1 | tee "$report_dir/latest.log"
