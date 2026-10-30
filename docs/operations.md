# Transfers and operations

Upgrade **both master and agents together**: file management now uses the
`ReadChunk`, `UploadChunk`, and `Archive` RPCs. Existing SQLite databases migrate
automatically to schema 14. Back up the master database before upgrading.

## Folder uploads and large downloads

The file manager supports folder selection and recursive directory drops.
Relative paths are preserved; drag/drop also preserves empty directories.
The confirmation dialog shows file count, total bytes, and destination collisions.
Choose overwrite, skip, or rename for the batch. The queue shows bytes transferred,
cancel, and retry. Transfers use 1 MiB chunks and disk staging on the agent; the
destination changes only when the complete upload is committed.

Retry queries the agent's saved offset before continuing. After reloading the
browser, select the same files and conflict policy again to resume. Browsers do
not retain permission to read local files across reloads. Cancel stops network
transfer and retains partial data for retry. Completed receipts and incomplete
uploads are stored in `<work_dir>.craftstack-uploads`, outside the file browser.
Administrators can remove inactive receipts/staged files when they are no longer
needed. Active uploads must not be cleaned up. The explicit DELETE chunks API
discards staging for an upload ID.

Downloads now fetch every chunk instead of using the editor's truncated 200 MiB
read response. Text editing still has a bounded preview; downloading does not.
Successful chunked uploads retain audit and best-effort Git history integration.

## ZIP files and deployments

Use **ZIP** in the file manager:

- `compress`: enter comma-separated source paths and a new output ZIP path;
  download the result using the existing file download action.
- `extract` / `deploy`: enter one uploaded ZIP path and a destination folder.
- `rollback`: supply a deployment ID shown after extraction/deployment.

Extraction and deployment require the instance to be stopped. Archives are first
expanded in staging, with traversal, symlink, duplicate-file, CRC, entry-count,
and expanded-size checks (100,000 entries / 100 GiB expanded). Before applying,
the full instance directory is snapshotted. Apply errors trigger restoration of
that snapshot. Rollback restores the **entire pre-deployment directory**, including
removing files introduced after that snapshot; this is not a per-file merge.
Snapshots live in `<work_dir>.craftstack-deployments` and require sufficient free
disk space for the snapshot and staged tree. Snapshot retention is manual.

The admin **운영 작업** (`/operations`) page lists deployment receipts and offers
rollback. This is a stopped-server deployment workflow, not rolling deployment
or Minecraft application-level readiness validation. Do not modify the same
directory externally while deploying or restoring it.

## Sync preview

Select a mapping on `/operations` and compare. Both agent and local sources are
supported. Each enabled target is compared by SHA-256. Results distinguish added,
modified, unchanged, and target-only files. Small text files (up to 8 KiB) show
before/after content; binary and larger files are hash-compared only.

The existing sync operation preserves target-only files; preview does not imply
they will be deleted. Destination directories must already exist. The preview is
read-only and reflects the files at comparison time; re-run it if files change.
It is not a transaction lock on the subsequent live sync operation.

## External backups

Set these environment variables on each **agent** to mirror all manual and
scheduled backups to an S3-compatible service using path-style Signature V4:

```text
CRAFTSTACK_BACKUP_S3_ENDPOINT=https://storage.example.com
CRAFTSTACK_BACKUP_S3_BUCKET=craftstack-backups
CRAFTSTACK_BACKUP_S3_REGION=us-east-1
CRAFTSTACK_BACKUP_S3_ACCESS_KEY=<access key>
CRAFTSTACK_BACKUP_S3_SECRET_KEY=<secret key>
CRAFTSTACK_BACKUP_S3_PREFIX=production/node-a
```

An empty endpoint disables mirroring. Credentials remain on the agent. The
endpoint must support path-style S3 and long-lived access keys. Configure remote
retention with the bucket's lifecycle policy. Local retention remains unchanged.
Use a different prefix per node. HTTPS is recommended; HTTP is supported for
local S3-compatible test services. Session-token credentials are not supported.

Before upload, CraftStack verifies the whole ZIP, each file's CRC, safe entry
paths, and SHA-256. A remote upload error reports failure and leaves the local
backup in place. This verifies archive integrity, not the consistency of a running
Minecraft world or database; stop/quiesce the application when a consistent
snapshot is required. For disaster recovery, download the object through your S3
client to the agent's backup location and use the existing restore workflow.

## Scheduled operations

Admins can schedule recurring restarts by interval (minutes) and first execution
time. The browser converts the selected local time to an absolute Unix time.
Minecraft operations send an optional `say` announcement and `save-all flush`,
then restart. All service types wait for three consecutive running-state reports,
three seconds apart, within a five-minute deadline. This checks process/container
state, not player login or plugin health. There is no announcement countdown.

Schedules persist in SQLite. Overlapping scheduled runs for an instance are
excluded. A master restart marks interrupted runs failed rather than repeating
the interrupted restart blindly. Missed recurring schedules run once after the
master resumes, then advance from the current time. The UI supports creation and
deletion; POST with an existing ID also supports edits and enabling/disabling.

## Per-instance access

Admins select a user, enable scoped access, and grant instance read/edit access.
Users without scoped access retain the existing global role behavior. An enabled
scope with no grants grants access to zero instances. Global Viewer remains
read-only even when an instance edit grant is present. Admin remains unrestricted.
Lists are filtered and direct API/page/WebSocket routes are checked. Shared
dashboard, audit, sync, node, and network pages require global access and are
unavailable to scoped users. Their home redirects to the instance list.

Saving permissions invalidates the user's sessions, including command submission
on an already-open console. The user must log in again. Scoped users can still
read their granted instance's console and files according to their role.

## Notifications and event history

The operations page configures an HTTP(S) webhook (Discord-compatible JSON with
`content` and mentions disabled). It records archive/deployment results, manual
and scheduled backups, scheduled restarts, node online/offline transitions,
observed crashed/failed states, and explicit sync target results.

Node/instance state is sampled every 15 seconds; transitions shorter than that
may not be observed. Notifications are sent for events, not unchanged polls.
Events persist in SQLite; unsuccessful delivery is retried up to five attempts.
Events created while no webhook is configured stay in history but are not later
replayed to a newly configured webhook. Repeated process crashes are reported as
observed transitions, rather than a dedicated restart-count threshold alert.

## Validation

Run `go test ./...`, `go vet ./...`, and `go build ./cmd/master ./cmd/agent`.
The regression suite covers resumable/atomic transfer, conflict behavior, empty
files, cancellation, reads beyond 200 MiB, directory traversal, ZIP deployment and
exact rollback, scoped route/list permissions, schedule validation/persistence,
sync comparisons, webhook delivery, S3 request handling, and backup restoration.
S3/Webhook tests use local HTTP test servers; real provider credentials and a
live Minecraft/Docker instance are required for deployment-environment acceptance.
