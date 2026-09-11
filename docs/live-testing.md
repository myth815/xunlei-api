# Real-device integration tests

Ordinary `go test ./...` never contacts your device. The tests in `tests/live` skip unless you explicitly enable writes. They use the public API, not private Xunlei endpoints.

Use a disposable parent download directory and a controlled, small HTTP(S) file that the **Xunlei container** can access. Keep the download slow enough for pause and resume to be observed; a fixture that finishes immediately cannot test those transitions. The harness refuses a reported file size above 64 MiB. A fixture in the 1–16 MiB range, served over approximately 30–90 seconds, is suitable. The fixture server should support the HTTP methods and byte ranges needed by Xunlei.

The API must already be running against an online, logged-in device. The parent ID comes from `GET /v1/directories`; it is an ID, not a filesystem path.

## Enable the lifecycle test

Set these variables in your local environment or secret manager:

| Variable | Meaning |
| --- | --- |
| `XUNLEI_API_LIVE_WRITE=1` | Explicitly enable test writes |
| `XUNLEI_API_TEST_URL` | Base URL of the independent API |
| `XUNLEI_API_TEST_KEY` | Its Bearer API key |
| `XUNLEI_API_TEST_PARENT_ID` | Writable disposable parent directory ID |
| `XUNLEI_API_TEST_SOURCE_URL` | Small, controlled slow HTTP(S) fixture URL |
| `XUNLEI_API_TEST_TIMEOUT` | Optional per-transition timeout; default `5m`, range `30s`–`30m` |

Run:

```sh
go test -count=1 -v -timeout=20m ./tests/live -run '^TestLiveTaskLifecycle$'
```

This creates a randomly named child directory and one task. It checks:

1. Creation returns a real task ID.
2. Repeating the exact same request and idempotency key returns the original operation and task.
3. A task read by ID reports the expected directory and unique filename.
4. Pause reaches `paused`, resume reaches `pending` or `running`, then download reaches `complete`.
5. `delete_files=false` removes the task record.

Without a filesystem mount, step 5 confirms record removal only. It cannot independently prove that downloaded files were preserved.

## Optional read-only file verification

Set `XUNLEI_API_TEST_MOUNT_PARENT` to an **absolute local path** that maps exactly to `XUNLEI_API_TEST_PARENT_ID`. The test reads only its newly created directory beneath that path. It does not trust task `real_path` fields and never removes files through the mount.

When this variable is present, the lifecycle test hashes the completed file before record removal and verifies the same file and SHA-256 remain afterwards. Optionally set `XUNLEI_API_TEST_SHA256` to the known fixture checksum to also verify downloaded content against a known value.

The test process needs only read access to the mount. The independent API itself does not need this mount.

## Separate file-deletion test

Set **both** `XUNLEI_API_LIVE_WRITE=1` and `XUNLEI_API_LIVE_DELETE_FILES=1`, then run:

```sh
go test -count=1 -v -timeout=20m ./tests/live -run '^TestLiveDeleteFiles$'
```

This creates a separate disposable directory and task, waits for completion, then sends `delete_files=true` for that task only. With the optional read-only mount, it checks that the test file existed before deletion and subsequently disappears. Without the mount, it reports only the upstream record/deletion indication; physical file deletion remains unverified.

The API's deletion operation can remain `accepted` or become `unknown_outcome` because it has no filesystem access. The harness's independent filesystem observation is separate evidence.

## Cleanup and uncertain outcomes

Cleanup tracks only task IDs returned by this run's create operation. Before any cleanup command, it re-reads the task and requires both its generated filename and new directory ID to match. It never enumerates tasks for bulk cleanup or touches a pre-existing task.

On failure, cleanup attempts to pause an active fixture and then remove its record with `delete_files=false`. Files and test directories are deliberately retained; the API does not expose directory removal. Their generated names are printed for manual inspection. Even the file-deletion test uses record-only cleanup when it fails.

If creation times out before a task ID is returned, the harness does not guess an ID, search for similar existing tasks, or create a replacement. Inspect the reported operation before starting a new run. Do not publish API keys, full task responses, or private source URLs in test reports.
