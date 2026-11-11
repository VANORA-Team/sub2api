#!/usr/bin/env bash
set -euo pipefail

# The caller checks out main with full history and the push credential.
# Overrides also allow the real merge procedure to be tested with local repos.
upstream_url=${UPSTREAM_URL:-https://github.com/ranxi2001/sub2api.git}
upstream_branch=${UPSTREAM_BRANCH:-production}
report=${SYNC_REPORT_DIR:-${RUNNER_TEMP:-/tmp}/upstream-sync}
mkdir -p "$report"
before=$(git rev-parse HEAD)

retry_fetch() {
  local attempt
  for attempt in 1 2 3; do
    if git fetch --no-tags "$upstream_url" "$upstream_branch"; then
      return 0
    fi
    if [[ "$attempt" != 3 ]]; then sleep "$((attempt * 2))"; fi
  done
  return 1
}

if [[ -n "$(git status --porcelain)" ]]; then
  echo "::error::Upstream sync requires a clean checkout."
  exit 1
fi
retry_fetch
upstream=$(git rev-parse FETCH_HEAD)
{
  echo "Main before sync: $before"
  echo "Upstream production: $upstream"
} | tee "$report/commits.txt"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  cat "$report/commits.txt" >> "$GITHUB_STEP_SUMMARY"
fi

build=false
if ! git merge-base --is-ancestor "$upstream" HEAD; then
  # Never choose an entire side of a source conflict automatically. Both
  # upstream fixes and fork behavior must survive the integration checks.
  if ! git merge --no-commit --no-ff "$upstream" 2>&1 | tee "$report/merge.log"; then
    git diff --name-only --diff-filter=U > "$report/conflicts.txt"
    git diff --cc > "$report/conflicts.patch"
    if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
      {
        echo
        echo '### Upstream merge needs resolution'
        echo '```text'
        cat "$report/conflicts.txt"
        echo '```'
        echo 'Main was not pushed. The upstream-sync-diagnostics artifact contains the conflict patch.'
      } >> "$GITHUB_STEP_SUMMARY"
    fi
    echo "::error::Upstream merge failed. See the summary and upstream-sync-diagnostics artifact; main has not been pushed."
    exit 1
  fi
  if [[ "${HAS_UPSTREAM_SYNC_TOKEN:-false}" != true ]] &&
    ! git diff --cached --quiet "$before" -- .github/workflows; then
    echo "::error::Upstream changes workflows. Configure UPSTREAM_SYNC_TOKEN with Contents and Workflows write permissions; main has not been pushed."
    exit 1
  fi
  git commit -m 'Merge upstream production into main'
  build=true
elif [[ "${FORCE:-false}" == true ]]; then
  build=true
fi

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "commit=$(git rev-parse HEAD)"
    echo "build=$build"
  } >> "$GITHUB_OUTPUT"
fi
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  echo "Main source: $(git rev-parse HEAD)" >> "$GITHUB_STEP_SUMMARY"
fi
