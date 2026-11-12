# Fork maintenance

`main` is the only maintained branch in `VANORA-Team/sub2api`. It is based on
`ranxi2001/sub2api`'s `production` history, with the fork's dependency security
fixes, CI compatibility changes, account pool quota monitoring, and image
automation retained.

`Sync upstream and build image` runs daily at 02:00 UTC (10:00 Asia/Taipei).
It fetches upstream `production`, merges into `main`, checks the backend and
frontend, pushes the successful merge to `main`, and builds the resulting commit.
It does not create branches. Fetches retry transient network failures. Merge
conflicts or failed checks stop the update before the push; resolve them while
retaining the fork changes. A failed merge lists the affected files in the run
summary and saves the merge log, commit IDs, and conflict patch as the
`upstream-sync-diagnostics` artifact (14 days).

Keep fork-only regression tests in separate files rather than adding assertions
to upstream tests. The November 10 and 11, 2026 failures came from overlapping
edits to shared probe and WebSocket tests. Pool quota snapshot coverage now lives
in `account_repo_pool_quota_probe_test.go`; the shared probe and WebSocket tests
follow upstream. The sync checks include both these paths and priority-analysis
cancellation. Source conflicts still require integration work; the job never
discards either side automatically.

The November 12, 2026 failure was another source merge conflict, this time in
the Grok model diagnostics API, settings search, and account-row tests. These
tests now follow upstream's request-options assertions, single traversal of
the smart-operations settings component, and external-link selector. The fork's
batch-create account regression remains covered. The sync frontend checks
include all three files so their combined behavior is checked before pushing.
The settings search registry also keeps just one OpenAI response-header timeout
entry; Git merged both sides' identical entries without reporting a conflict.
The fork's Antigravity monitoring call passes `force=false` to upstream's new
usage-fetch signature so routine monitoring continues to honor its cache.

Run `python3 .github/scripts/test_upstream_sync.py` to check the merge and image
recovery procedure with local Git histories and an isolated registry stub.
`Upstream sync automation checks` also runs these checks when the sync tooling
changes in a push or pull request.

For unattended updates that change `.github/workflows`, configure the repository
Actions secret `UPSTREAM_SYNC_TOKEN` with a fine-grained personal access token
limited to this repository, granting **Contents: Read and write** and
**Workflows: Read and write**. The checkout action uses this token for Git
operations. Without it, the job falls back to `GITHUB_TOKEN`, which can push
ordinary source changes but cannot push changes to workflow files, even with
`contents: write`. If a merge changes workflows while this secret is missing,
the job stops before builds and explains how to configure it. Repository policy
currently disables SSH deploy keys. Do not copy a developer's GitHub CLI login
token into Actions. Image publication continues to use the job's `GITHUB_TOKEN`
with `packages: write`.

The image is `ghcr.io/vanora-team/sub2api:sha-<full main commit>`, with amd64 and
arm64 variants. Every successful sync checks whether that commit already has
both architectures, even when upstream has no new commits. A missing or
incomplete image is built again, so a failed publication is recoverable on the
next run. Registry authentication or network errors remain visible failures.
The `force` input rebuilds an existing image as well. The separate `Build fork
image` workflow can rebuild `main` manually without syncing upstream. OCI
revision labels identify the source commit. Publication does not deploy a
running server.

All application Dockerfiles set `SUB2API_UPDATE_MODE=external`. In this mode,
the version panel links to `https://github.com/VANORA-Team/sub2api` and explains
how to replace the image. Version requests do not contact GitHub or reuse
upstream release caches, and the update and rollback APIs cannot replace the
container's executable. To update or roll back, pull the desired
`ghcr.io/vanora-team/sub2api:sha-<full main commit>` image and recreate the
container. Standalone builds use this fork's releases and reject cached update
metadata from other repositories.

The initial branch cleanup was backed up in
`.local/branch-rebuild-20261106/branches-before.bundle`, with the original branch
heads in `refs-before.txt`. These local artifacts are excluded from Git.
