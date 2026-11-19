#!/usr/bin/env bash
set -euo pipefail

report_dir=.state/v1-soak
mkdir -p "$report_dir"
export REGATE_SOAK_DURATION="${REGATE_SOAK_DURATION:-6m}"

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
    go test -race ./internal/server -run '^TestV1Soak$' -count=1 -timeout 20m -v
}

printf 'Mixed-load report: %s/latest.log\n' "$report_dir"
run 2>&1 | tee "$report_dir/latest.log"
