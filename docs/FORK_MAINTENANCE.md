# Fork maintenance

`main` is the only maintained branch in `VANORA-Team/sub2api`. It is based on
`ranxi2001/sub2api`'s `production` history, with the fork's dependency security
fixes, CI compatibility changes, account pool quota monitoring, and image
automation retained.

`Sync upstream and build image` runs daily at 02:00 UTC (10:00 Asia/Taipei).
It fetches upstream `production`, merges into `main`, checks the backend and
frontend, pushes the successful merge to `main`, and builds the resulting commit.
It does not create branches. Merge conflicts or failed checks stop the update
before the push; resolve them while retaining the fork changes.

The image is `ghcr.io/vanora-team/sub2api:sha-<full main commit>`, with amd64 and
arm64 variants. The separate `Build fork image` workflow can rebuild `main`
manually without syncing upstream. OCI revision labels identify the source
commit. Publication does not deploy a running server.

The initial branch cleanup was backed up in
`.local/branch-rebuild-20261106/branches-before.bundle`, with the original branch
heads in `refs-before.txt`. These local artifacts are excluded from Git.
