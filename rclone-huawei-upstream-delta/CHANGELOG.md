# Complete local change record

Baseline: official rclone commit `9e0a5b66a4a959ff9056ec2c76d168679d48051e` (`v1.75.0` tree).

## A. Files.list pagination correctness

Problem:

- Huawei Files.list documents and returns `nextCursor`; the old backend primarily followed `nextPageToken`.
- In production this caused incomplete directory visibility (effectively only the first page), which then caused rclone to treat existing remote objects as missing and create duplicate/auto-renamed objects.
- Huawei's documented `pageSize` maximum is 100, not 1000.

Changes:

- Add `NextCursor string json:"nextCursor,omitempty"` to `api.FileList`.
- Change `list_chunk` default/help from 1000 to 100.
- Clamp requested list chunk to 100.
- Prefer `nextCursor` and send it as `cursor` on the next request.
- Retain `nextPageToken`/`pageToken` only as a compatibility fallback.
- Detect repeated cursor/token values and fail instead of looping forever.
- Use server-side encoded filename filters for `FindLeaf` and `readMetaDataForPath`, reducing full-directory scans and narrowing race windows.

Tests:

- `TestFileListNextCursor`
- `TestListDirectoryUsesNextCursorAndPageSize100`

## B. autoRename request placement and duplicate safety

Problem:

- The prior code serialized numeric `autoRename=3` into JSON/multipart metadata.
- Huawei Files.create and Files.create.resume require boolean `autoRename=false` as a URL query parameter.
- Incorrect placement allowed duplicate names to be silently auto-renamed, producing `file (1)` style copies.
- Zero-byte upload had a separate path and previously retained the wrong JSON behavior.

Changes:

- Remove `AutoRename` from `api.CreateFolderRequest` so it cannot be serialized accidentally.
- Remove JSON/multipart `autoRename=3` metadata.
- Add URL query `autoRename=false` to:
  - zero-byte `uploadEmpty` create;
  - multipart create;
  - resumable create initialization.
- Update comments/error interpretation to reflect boolean query behavior.

Tests:

- `TestCreateFolderRequestDoesNotSerializeAutoRename`
- `TestUploadEmptyUsesAutoRenameQuery`
- resumable test server also asserts the query value.

## C. Resumable upload slice negotiation and HTTP 308 state machine

Problem:

- Local chunk size could differ from Huawei's negotiated `sliceSize` (observed 8 MiB client vs 10 MiB server), causing corrupt/incomplete resumable sessions.
- HTTP 308 means resumable upload is incomplete/progress state, not a generic transient error.
- Retrying the same request generically can deadlock, duplicate bytes, or omit a partially accepted suffix.
- Finalization/status polling needs to distinguish a status query from an actual suffix resend.

Changes:

- Use `ResumeUploadInitResponse.SliceSize`; fall back to configured chunk size only if the server returns a non-positive value.
- Add `resumeOffsetFromRange`, parsing cumulative `Range: bytes=0-N` (also accepts `bytes 0-N`) and returning `N+1`.
- Reject missing, malformed, non-zero-start, out-of-bounds, regressing, or cross-chunk Range state. Data integrity is preferred over guessing.
- Buffer each current chunk and resend only the unacknowledged suffix inside that chunk.
- Fail on unexpected short source/EOF instead of silently uploading less data.
- Treat 308 before generic retry handling; do not put 308 in common retry codes.
- Keep chunk `Content-Range: bytes start-end/total` accurate for initial and suffix sends.
- Perform final zero-length status query without invented protocol headers; if the returned Range proves the last chunk is partial, resend the exact suffix with a correct Content-Range.
- Poll final state with context cancellation and bounded delay; return explicit incomplete-upload errors.
- Add diagnostic debug logging of requested chunk size, negotiated slice size, and total size.

Tests:

- `TestResumeOffsetFromRange`
- `TestUploadResumeResendsCurrentChunkSuffix`
- `TestUploadResumeFinalQueryResendsLastChunkSuffix`
- `TestUploadResumeRejectsMissingRange`
- `TestUploadResumeRejectsShortSource`

Controlled real API evidence:

- zero-byte create succeeded;
- 1 MiB upload/download matched;
- 24 MiB resumable upload received a final 308 one byte short, resent exactly the last byte, then completed;
- downloaded SHA256 and byte-for-byte `cmp` matched local input.

## D. Huawei MP4 SHA256 metadata exception

Problem:

- Controlled tests used identical large bytes uploaded under different name/MIME combinations.
- Only `video/mp4` MIME together with `.mp4` remote extension produced an API SHA256 differing from both local bytes and downloaded bytes.
- Downloaded content remained byte-for-byte correct; therefore the metadata hash is unreliable for that exact combination and must not trigger false corruption/reupload loops.

Changes:

- In `Object.Hash`, parse the media type.
- When MIME is `video/mp4` and remote extension is `.mp4` (case-insensitive), return an empty hash with no error.
- Preserve SHA256 behavior for every other type and combination.

Tradeoff:

- This prevents false `corrupted on transfer` failures but means rclone cannot use Huawei's API SHA256 to detect genuine corruption for those MP4 objects. Size checks remain; targeted download-and-compare is required for high-assurance MP4 verification.

Test:

- `TestObjectHashSkipsMP4Metadata`

## E. Verification inventory

Generated patch includes exactly:

```text
backend/huaweidrive/api/types.go        |   2 +-
backend/huaweidrive/api/types_test.go   |  20 ++
backend/huaweidrive/huaweidrive.go      | 316 +++++++++++++++++++++---------
backend/huaweidrive/huaweidrive_test.go | 334 ++++++++++++++++++++++++++++++++
4 files changed, 582 insertions(+), 90 deletions(-)
```

Required future regression gates:

1. 101+ object directory proves cursor pagination beyond the first 100.
2. Zero-byte, multipart, and resumable create prove `autoRename=false` exists only in URL query.
3. Resumable test covers server slice size differing from configured chunk size.
4. 308 partial current chunk resends only the suffix.
5. 308 final query can recover a partial last chunk.
6. Missing/invalid Range fails safely.
7. Short source fails safely.
8. MP4 MIME+extension returns no metadata hash; other combinations retain SHA256.
