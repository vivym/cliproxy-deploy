# CLIProxyAPI Log Archive To Cloudflare R2 Runbook

This runbook sets up safe, low-impact archiving for CLIProxyAPI full request logs.

Use it only during short troubleshooting windows where `request-log: true` is enabled. Full request logs can include prompts, request bodies, response bodies, headers, streaming chunks, upstream API data, and secrets users typed into requests.

## Target Behavior

- CLIProxyAPI writes one request log file per request under `logs/`.
- `scripts/archive-cliproxy-logs.sh` skips fresh files and files still opened by a process.
- Old request log files are compressed with low CPU impact: `gzip -1`, `nice -n 19`, and idle `ionice` when available.
- Compressed `.gz` files are uploaded to Cloudflare R2.
- Successful uploads get a local `.uploaded` marker.
- Uploaded local `.gz` files are deleted after `CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1`.
- Upload failures leave local files in place for the next run.

## Prerequisites

On the server:

```bash
cd /opt/cliproxy-deploy
test -f .env
test -d logs
command -v aws
```

Install AWS CLI v2 if needed. On Ubuntu/Debian, follow the AWS CLI v2 package instructions from AWS. The script does not require `aws configure`; it reads R2 credentials from `.env`.

Optional tools:

```bash
sudo apt-get update
sudo apt-get install -y lsof cpulimit
```

`lsof` lets the script skip open files. `cpulimit` is only needed when you set `CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT`.

## Cloudflare R2 Setup

1. Create or choose an R2 bucket.
2. Create an R2 API token with object read/write permission for that bucket.
3. Record:
   - Cloudflare account ID.
   - Bucket name.
   - R2 access key ID.
   - R2 secret access key.

The script uses this endpoint by default:

```text
https://<R2_ACCOUNT_ID>.r2.cloudflarestorage.com
```

Set `R2_ENDPOINT_URL` only if you need to override it.

## Configure `.env`

Add or update:

```env
R2_ACCOUNT_ID=replace-with-cloudflare-account-id
R2_BUCKET=replace-with-r2-bucket
R2_ACCESS_KEY_ID=replace-with-r2-access-key-id
R2_SECRET_ACCESS_KEY=replace-with-r2-secret-access-key

CLIPROXY_LOG_ARCHIVE_R2_PREFIX=cliproxy-logs
CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES=30
CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1

CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL=1
CLIPROXY_LOG_ARCHIVE_NICE=19
CLIPROXY_LOG_ARCHIVE_IONICE_IDLE=true
CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT=
```

For weak CPUs, start with the defaults above. If compression still hurts the server, install `cpulimit` and set:

```env
CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT=25
```

Use a value from `1` to `100`. Keep the value empty to avoid the `cpulimit` dependency.

## Validate R2 Credentials

Run a direct R2 write/read/delete test:

```bash
set -a
source .env
set +a

tmpfile="$(mktemp)"
printf 'r2 archive test\n' > "$tmpfile"

AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
AWS_DEFAULT_REGION="${R2_REGION:-auto}" \
aws s3 cp "$tmpfile" "s3://${R2_BUCKET}/cliproxy-logs/_healthcheck.txt" \
  --endpoint-url "https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"

AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
AWS_DEFAULT_REGION="${R2_REGION:-auto}" \
aws s3 rm "s3://${R2_BUCKET}/cliproxy-logs/_healthcheck.txt" \
  --endpoint-url "https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"

rm -f "$tmpfile"
```

If this fails, fix R2 token scope, bucket name, account ID, or network egress before enabling the archive job.

## Test The Archive Script Safely

Use a temporary log directory first:

```bash
tmpdir="$(mktemp -d)"
printf 'sample request log\n' > "${tmpdir}/request-test.log"
touch -d '2 hours ago' "${tmpdir}/request-test.log"

CLIPROXY_LOG_ARCHIVE_DIR="$tmpdir" \
CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES=30 \
scripts/archive-cliproxy-logs.sh

find "$tmpdir" -maxdepth 1 -type f -print
```

Expected local files after a successful run:

```text
request-test.log.gz
request-test.log.gz.uploaded
```

Expected R2 object:

```text
s3://<R2_BUCKET>/cliproxy-logs/request-test.log.gz
```

Clean up:

```bash
rm -rf "$tmpdir"
```

## Enable Scheduled Runs

Cron example:

```cron
*/10 * * * * cd /opt/cliproxy-deploy && scripts/archive-cliproxy-logs.sh >> /var/log/cliproxy-log-archive.log 2>&1
```

Install it with:

```bash
crontab -e
```

The 10-minute cadence is conservative. The script is idempotent:

- Already uploaded `.gz` files are skipped because they have `.uploaded` markers.
- Failed uploads are retried later.
- Local uploaded copies are cleaned after `CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1`.

## Enable Full Request Logging Temporarily

Keep the production default:

```yaml
request-log: false
```

For a troubleshooting window:

1. Change `config.yaml` to `request-log: true`.
2. Restart CLIProxyAPI:

   ```bash
   docker compose restart cliproxyapi
   ```

3. Reproduce the issue.
4. Confirm archive job activity:

   ```bash
   tail -n 100 /var/log/cliproxy-log-archive.log
   find logs -maxdepth 1 -name '*.gz.uploaded' | tail
   ```

5. Turn logging back off:

   ```yaml
   request-log: false
   ```

6. Restart CLIProxyAPI again:

   ```bash
   docker compose restart cliproxyapi
   ```

## Verify Ongoing Health

Check cron output:

```bash
tail -n 200 /var/log/cliproxy-log-archive.log
```

List recent uploaded objects:

```bash
set -a
source .env
set +a

AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
AWS_DEFAULT_REGION="${R2_REGION:-auto}" \
aws s3 ls "s3://${R2_BUCKET}/${CLIPROXY_LOG_ARCHIVE_R2_PREFIX:-cliproxy-logs}/" \
  --endpoint-url "https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com" \
  --recursive \
  | tail
```

Check local cleanup:

```bash
find logs -maxdepth 1 -name '*.gz' -mtime +1 -print
find logs -maxdepth 1 -name '*.gz.uploaded' -mtime +1 -print
```

These should usually be empty after the next scheduled run.

## Troubleshooting

`Missing required command: aws`

Install AWS CLI v2 on the host.

`Missing required command: cpulimit`

You set `CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT`, but `cpulimit` is not installed. Install `cpulimit` or clear the variable.

`AccessDenied` or `InvalidAccessKeyId`

Check `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, bucket permission, and whether the token is scoped to the selected bucket.

`NoSuchBucket`

Check `R2_BUCKET` and Cloudflare account ID.

Compression still uses too much CPU:

```env
CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL=1
CLIPROXY_LOG_ARCHIVE_NICE=19
CLIPROXY_LOG_ARCHIVE_IONICE_IDLE=true
CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT=15
```

Then rerun:

```bash
scripts/archive-cliproxy-logs.sh
```

R2 upload works but local files are not deleted:

- Confirm `.uploaded` markers exist.
- Confirm marker mtime is older than `CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS`.
- Run the script again; cleanup happens after upload processing.

## Security Notes

- Compression is not encryption.
- R2 upload does not make full request logs safe to expose.
- Keep the R2 bucket private.
- Prefer short troubleshooting windows and turn `request-log` back off quickly.
- Consider R2 lifecycle rules for server-side retention, for example deleting `cliproxy-logs/` objects after a short period.
