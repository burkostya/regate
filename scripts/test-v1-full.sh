#!/usr/bin/env bash
set -uo pipefail

report_dir=.state/v1-acceptance
mkdir -p "$report_dir" || exit 1

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
    npm --prefix web run build || return
    REGATE_V1_ACCEPTANCE_BROWSER=1 go test -race ./internal/server -run '^TestV1Acceptance$' -count=1 -v
}

printf 'Acceptance report: %s/latest.log\n' "$report_dir"
run 2>&1 | tee "$report_dir/latest.log"
