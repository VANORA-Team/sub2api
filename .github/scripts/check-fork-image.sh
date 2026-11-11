#!/usr/bin/env bash
set -euo pipefail

# A previous run may have pushed main and then failed to publish its image.
# Inspect the commit tag on every sync, including when there is nothing to merge.
: "${IMAGE:?IMAGE is required}"
build=true
if [[ "${FORCE:-false}" != true ]]; then
  manifest=$(mktemp)
  errors=$(mktemp)
  trap 'rm -f "$manifest" "$errors"' EXIT
  if docker buildx imagetools inspect --raw "$IMAGE" > "$manifest" 2> "$errors"; then
    if "${PYTHON:-python3}" - "$manifest" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    manifest = json.load(handle)
platforms = {
    (entry.get("platform", {}).get("os"), entry.get("platform", {}).get("architecture"))
    for entry in manifest.get("manifests", [])
}
sys.exit(0 if {("linux", "amd64"), ("linux", "arm64")} <= platforms else 1)
PY
    then
      build=false
    fi
  elif ! grep -Eqi 'manifest unknown|name unknown|not found' "$errors"; then
    cat "$errors" >&2
    echo "::error::Cannot inspect the image registry. Retry this run after registry access recovers."
    exit 1
  fi
fi
echo "build=$build" >> "$GITHUB_OUTPUT"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  if [[ "$build" == true ]]; then
    echo "Building image: $IMAGE" >> "$GITHUB_STEP_SUMMARY"
  else
    echo "Image already published for linux/amd64 and linux/arm64: $IMAGE" >> "$GITHUB_STEP_SUMMARY"
  fi
fi
