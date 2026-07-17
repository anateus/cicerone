# Cache and recovery

Cicerone stores its durable feed, installed snapshot, preferences, and changelog artifacts at:

```text
~/Library/Application Support/cicerone/cicerone.db
```

It does not automatically delete or rebuild a database that fails to open or migrate. The error includes the resolved database path and recovery commands. Stop Cicerone before working on the file.

## 1. Make a safe copy

Preserve metadata and leave the original untouched:

```sh
db="$HOME/Library/Application Support/cicerone/cicerone.db"
cp -p -- "$db" "$db.recovery-copy"
```

If present, also copy the sibling `cicerone.db-wal` and `cicerone.db-shm` files before inspecting anything. Work on the copy, not the original.

## 2. Check integrity

```sh
sqlite3 "$db.recovery-copy" 'PRAGMA integrity_check;'
```

Healthy output is exactly `ok`. Any other output describes corruption; keep every original file unchanged.

## 3. Export readable data

Try a logical export from the copy:

```sh
sqlite3 "$db.recovery-copy" .dump > cicerone-recovery.sql
```

Review the command's exit status and stderr. A partial dump can still be useful, but do not import it over the original. Advanced SQLite recovery may be attempted separately with `sqlite3 .recover` if the installed CLI supports it.

## 4. Rebuild only by explicit choice

Rebuilding discards cached feed history, changelogs, sync cursors, and preferences. Homebrew packages are not removed or changed. Preserve the failed database, then allow Cicerone to create a new one:

```sh
mv -- "$db" "$db.corrupt"
mv -- "$db-wal" "$db-wal.corrupt" 2>/dev/null || true
mv -- "$db-shm" "$db-shm.corrupt" 2>/dev/null || true
./cicerone
```

The first restart needs Homebrew/Git access to repopulate history. Keep the preserved files until the rebuilt cache has been checked. Cicerone-owned Git mirrors live under `~/Library/Caches/cicerone/`; they are independent of the SQLite recovery procedure and need not be removed.
