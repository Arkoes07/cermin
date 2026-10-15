# Runbook

Restoring the database from its R2 backup ([ADR-0005](adr/0005-fly-single-machine-r2-backups.md)). Written from the restore drill on 2026-10-15 ([#4](https://github.com/Arkoes07/cermin/issues/4)), where these commands ran, except the fallback, which wasn't needed.

## Setup, once

**Litestream 0.5.17**, the version in the [Dockerfile](../Dockerfile). 0.3 can't read 0.5's backups, and Homebrew has no formula, so take it from the release and put `litestream` on your `PATH`:

```sh
gh release download v0.5.17 -R benbjohnson/litestream -p 'litestream-0.5.17-darwin-arm64.tar.gz'
tar -xzf litestream-0.5.17-darwin-arm64.tar.gz litestream
```

**The R2 key in `.env`** (git-ignored): `LITESTREAM_ACCESS_KEY_ID`, `LITESTREAM_SECRET_ACCESS_KEY`, `R2_ENDPOINT`, `R2_BUCKET`. They are Fly secrets, and `fly secrets list` shows only digests. Read them off the machine in your own terminal, then write them as `KEY=value` lines:

```sh
fly ssh console -C 'printenv LITESTREAM_ACCESS_KEY_ID LITESTREAM_SECRET_ACCESS_KEY R2_ENDPOINT R2_BUCKET'
```

## Count Thoughts

```sh
fly ssh console -C "sqlite3 -readonly /data/cermin.db 'select count(*), max(id) from thoughts'"
```

With two machines, add `--machine <id>`. `-s` doesn't take an ID: it means pick one from a list.

## Restore locally

```sh
set -a; . ./.env; set +a
CERMIN_DB_PATH=/data/cermin.db litestream restore -config litestream.yml -o data/restore-test.db /data/cermin.db
sqlite3 -readonly data/restore-test.db 'pragma integrity_check; select count(*), max(id) from thoughts'
```

`CERMIN_DB_PATH` names the database in [litestream.yml](../litestream.yml), so the replica settings stay in one place. The restore prints nothing when it succeeds. Add `-force` to overwrite an earlier copy.

## Rebuild from an empty volume

For a lost volume, or a drill. On boot, Litestream restores `/data/cermin.db` from R2 when the volume has none, then starts `cermin serve`. Fly can't swap the volume under a machine, so this boots a new machine on a new empty volume and keeps the old pair as the fallback until the new one has proved itself.

Don't push to `main` until step 6. A push deploys, and while two volumes are named `cermin_data`, a deploy could mount the wrong one.

1. Note the old machine and volume with `fly machine list` and `fly volumes list`. If the old machine still runs, [count its Thoughts](#count-thoughts).
2. Stop it: `fly machine stop <old>`. Two running machines would both poll Telegram and both replicate to the same R2 path, which can corrupt the backup. Telegram holds messages while the bot is down.
3. `fly machine clone <old> --region iad`. It creates a new, empty 1 GB `cermin_data` volume and waits for the health check, about a minute. Don't pass `--from-snapshot`: it copies the old disk.
4. Check the restore. `fly logs --no-tail -i <new>` shows `restore completed` before `capturing Thoughts` (7 s in the drill). The count and newest ID on the new machine match step 1. `fly status` shows the check passing.
5. Send the bot a Thought. It gets an Acknowledgement and shows up in the new machine's database.
6. Only then: `fly machine destroy <old>`, then `fly volumes destroy <old volume> --yes`. `fly volumes list` shows one `cermin_data`, attached to the new machine.

If a check fails before step 6, nothing is lost: `fly machine destroy <new> --force`, `fly volumes destroy <new volume> --yes`, then `fly machine start <old>`.
