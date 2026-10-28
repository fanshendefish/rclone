# rclone Huawei Drive local patch — maintenance record

Generated: 2026-07-14

This directory is the canonical handoff package for carrying the local Huawei Drive fixes onto future official rclone versions.

## 1. Verified official baseline

- Official repository: `https://github.com/rclone/rclone.git`
- Official commit: `9e0a5b66a4a959ff9056ec2c76d168679d48051e`
- Commit date: `2026-07-12T17:10:10+01:00`
- Commit author: Nick Craig-Wood `<nick@craig-wood.com>`
- Commit subject: `march: fix unnecessarily listing dst directory when src listing finished`
- Official tree: `4972622c0b10b048ed768b995c3bf3da730beb5f`
- Version file at baseline: `v1.75.0`

The official commit was fetched directly from GitHub into `/tmp/rclone-upstream-verify`. The old local repository had a synthetic `base` commit (`d926d5cfe2f87392cd8bdccf47c7769ecb4790f0`) whose tree is exactly the same official tree (`4972622...`). Therefore the patch is now anchored to the real official commit, not to the synthetic local history.

## 2. Canonical artifacts

- `0001-huaweidrive-correctness.patch`: complete unified Git patch against the official commit above. This is the primary update artifact.
- `rclone-official-9e0a5b66-source.tar.gz`: complete clean official rclone source tree exported from that exact GitHub commit.
- `source-before/`: complete original versions of every modified official source file.
- `source-after/`: complete patched versions, including the newly added test file.
- `CHANGELOG.md`: behavioral rationale, affected code paths, test evidence, and future-porting rules.
- `SHA256SUMS.txt`: integrity hashes for every handoff artifact.

The patch contains all changes and only these four paths:

```text
backend/huaweidrive/api/types.go
backend/huaweidrive/api/types_test.go       (new)
backend/huaweidrive/huaweidrive.go
backend/huaweidrive/huaweidrive_test.go
```

Patch size summary:

```text
4 files changed, 582 insertions(+), 90 deletions(-)
```

## 3. Apply to the exact baseline

```sh
git clone https://github.com/rclone/rclone.git
cd rclone
git checkout --detach 9e0a5b66a4a959ff9056ec2c76d168679d48051e
git apply --check /path/to/0001-huaweidrive-correctness.patch
git apply /path/to/0001-huaweidrive-correctness.patch
go test ./backend/huaweidrive/...
go test -race ./backend/huaweidrive/...
go vet ./backend/huaweidrive/...
```

`git apply --check` and `go test ./backend/huaweidrive/...` were run successfully in a fresh official worktree while generating this package.

## 4. Port to a newer official version

Do not overwrite new upstream files with `source-after`. Use the patch as a semantic change set:

1. Clone/fetch the new official release and create a dedicated branch.
2. Record the new upstream commit and tree hash.
3. Try `git apply --3way 0001-huaweidrive-correctness.patch`.
4. Resolve conflicts by behavior described in `CHANGELOG.md`, not by blindly choosing the local side.
5. Check whether upstream has independently fixed each item. If so, retain upstream's implementation and port only missing tests/behavior.
6. Run all Huawei backend tests, race tests, vet, and controlled real API tests.
7. Build for Synology with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`.
8. Compare the built binary hash, deploy with a backup, and perform staged upload/download verification before production bisync.

High-conflict functions to inspect on every update:

- `Fs.FindLeaf`
- `Fs.listDirectoryWithFilter`
- `Object.Hash`
- `Fs.readMetaDataForPath`
- `Object.uploadEmpty`
- `Object.uploadMultipart`
- `Object.uploadResume`
- `api.FileList`
- `api.CreateFolderRequest`

## 5. Non-code production blocker

The source patch is validated, but production recurring bisync is currently disabled because the NAS root filesystem is full and cannot create `.lst-new`. See:

`/Users/qinchuan/Documents/NAS华为云同步/AI-HANDOFF-20260714.md`

Do not interpret source correctness as permission to re-enable Task 10 before fixing the rclone cache/work-state location and completing a successful normal bisync run.
