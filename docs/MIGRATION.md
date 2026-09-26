# Migrating a running deployment from Elasticsearch to PostgreSQL

Why: [ADR 0001](adr/0001-postgres-system-of-record.md). This is the cut-over procedure for a
deployment that has data in the legacy Elasticsearch indices (`user`, `follow`, `post`,
`like`, `share`, `comment`, `message`, `notification`). A fresh install needs none of it:
`docker compose up` creates the schema and an empty search index.

## Strategy: short write freeze, not dual writes

Two ways to move a live store:

- **Dual write** (write both stores, backfill, compare, switch reads, stop the old writes) keeps
  the site writable throughout, but needs a second code path per entity, a comparison job,
  and a plan for the writes that land in one store and not the other.
- **Write freeze** (reject writes, copy, verify, switch) is one code path and easy to reason
  about, at the price of minutes of read-only time.

At this data size the freeze is the right trade. It becomes the wrong one when the copy takes
longer than an acceptable read-only window — measure it in the rehearsal (step 1) before
committing to it.

## Tools

| Command | What it does | Safe to re-run |
|---|---|---|
| `migrate` | applies `migrations/*.sql` | yes (applied files are skipped; edited ones refuse to run) |
| `backfill` | copies every legacy index into PostgreSQL, recounts likes/shares, carries over post events still pending in the old outbox, prints a report | yes (every insert is keyed by the legacy id, `ON CONFLICT DO NOTHING`) |
| `reindex` | builds the post search index (alias `posts`) from PostgreSQL | yes (versioned writes) |

All three are in the `cmd/Dockerfile` image: `docker compose run --rm migrate ./backfill`.

## Procedure

1. **Rehearse on a copy.** Restore the latest Elasticsearch snapshot into a scratch cluster and
   run steps 4–6 against an empty database. Record the backfill duration: that is the freeze
   window. Read the report's `rejected` counts and reasons (see "Reading the report").

2. **Snapshot Elasticsearch** (the rollback point).

3. **Freeze writes.** Put the gateway in read-only mode (return `503` for anything but `GET`),
   then stop the old post-service instances *gracefully*: SIGTERM makes them flush pending
   counter deltas and finish their outbox pass. Stop the old workers once their consumer lag
   is zero.

4. **Create the schema and copy the data.**

   ```bash
   docker compose up -d postgres
   docker compose run --rm migrate ./migrate
   docker compose run --rm migrate ./backfill > backfill-report.json
   ```

5. **Verify** before anything writes to PostgreSQL:
   - For every entity, `read == inserted + existing + rejected` in the report, and the
     rejections are the kinds seen in the rehearsal.
   - Spot checks against the old indices: a few users' follower counts, a few posts' like
     counts, the newest messages of a busy conversation.
   - `SELECT count(*) FROM outbox WHERE status = 'pending'` equals the number of posts whose
     old `outbox_status` was `pending` (they will be published by the new relay).

6. **Build the search index.**

   ```bash
   docker compose run --rm migrate ./reindex
   ```

   Posts copied with an embedding keep it; the others get one from the indexer's repair loop
   once `OPENAI_API_KEY` is set.

7. **Start the new services and lift the freeze.** Watch `outbox_pending`,
   `outbox_oldest_pending_seconds`, consumer DLQ counters and 5xx rates.

8. **Clean up** after a week without rollback: delete the Redis `like_set:*` keys (no longer
   used), then the legacy indices. Home feeds (`feed:home:*`), counters and idempotency keys
   carry over unchanged: post ids did not change.

## Rollback

- **Before step 7**, nothing has written to PostgreSQL: restart the old services and lift the
  freeze. Elasticsearch was never modified.
- **After step 7**, new writes exist only in PostgreSQL. Rolling back would lose them, which is
  why step 5 happens during the freeze. A forward fix is the default from here.

## Reading the report

The old store enforced none of the new constraints, so some rows are refused — by design:

| Reason | Typical cause | Action |
|---|---|---|
| `references a missing row` | a like, share or comment on a post whose document was removed | none; the row was unreachable already |
| `violates a check constraint` | a self-follow, an empty or over-long comment or message | none, or fix by hand and re-run |
| `unreadable document` | a document missing required fields | inspect by id in the old index |
| `existing` > 0 on the first run | duplicate documents (e.g. a follow stored twice by the old race) | none; they collapse into one row |
