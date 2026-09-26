# ADR 0001: PostgreSQL as the system of record; Elasticsearch as a search projection

- **Status:** accepted, implemented
- **Date:** 2026-09
- **Deciders:** backend
- **Supersedes:** the original design, where Elasticsearch stored every entity

## Context

Every entity — users, follows, posts, likes, shares, comments, messages, notifications — was a
document in its own Elasticsearch index. Elasticsearch is a search engine with near-real-time
visibility, per-document atomicity and no transactions, joins, foreign keys or unique
constraints other than the document id. The code showed what that costs:

| Need | What the code had to do | What went wrong |
|---|---|---|
| Unique user id, one follow per pair, one like per user | search, then write | concurrent requests both passed the search: duplicate follows, a second sign-up overwriting the first user's password. Likes needed Redis `SADD` *and* `op_type=create` to be safe |
| Post + its event, atomically | embed the outbox inside the post document | worked, but the relay polled the post index for `outbox_status`, and nothing else could share the trick (the `post.liked` event stayed best-effort) |
| "All followers", "the whole conversation" | search requests without a size | silently truncated to the default 10 hits |
| A conversation in order | an `OR` of two term queries, sorted in memory | unbounded memory, no paging, and time is not a safe paging key anyway |
| Counters on a hot post | update-by-script on one document | version conflicts under load, 500s (worked around with a write-behind counter) |
| Referential integrity | none | likes and comments on posts that no longer exist |
| Point-in-time recovery | snapshots only | no replay to "just before the bad deploy" |

The data is relational (a follow graph, likes as a join between users and posts, threads of
comments) and small per user. The workload that genuinely needs Elasticsearch is full-text
and vector search over posts.

## Decision

1. **PostgreSQL is the system of record** for users, follows, posts, likes, shares, comments,
   conversations, messages, notifications and the outbox. Uniqueness and integrity are
   constraints (primary keys, composite foreign keys, CHECKs), not read-then-write code.
2. **Elasticsearch keeps one index: a projection of posts** for keyword and kNN search. It is
   written only by the search-indexer and read only for ranking; results are re-read from
   PostgreSQL before they are returned.
3. **Synchronisation is event-driven through the transactional outbox**, not log-based CDC:
   the `outbox` table is written in the business transaction and published by a polling relay
   (`FOR UPDATE SKIP LOCKED` claims, a lease, backoff). Events carry ids; the indexer reads the
   current row and writes it with `version_type=external`, so ordering and duplicates do not
   matter.
4. **Messages** are keyed by `(conversation_id, seq)` with a per-conversation sequence assigned
   under the conversation's row lock, and paged by `seq`.
5. **One database, table ownership per service.** Each table has one writing service; others
   may read through `shared/socialgraph`. Foreign keys never cross an ownership line.
6. **Schema changes are versioned SQL** applied by a one-shot `migrate` job, not by services at
   startup.

## Alternatives considered

**Keep Elasticsearch and fix the races.** `op_type=create` with deterministic ids fixes
follow and sign-up uniqueness, and sizes/scrolls fix truncation. It does not give
transactions across documents (a like and its event, a message and its conversation
counter), foreign keys, or ordered paging by commit order. We would keep re-implementing a
database in application code.

**Debezium (log-based CDC) instead of a polling relay.** Reads the WAL, so no polling load, no
lease tuning, and it also captures changes made outside the application (a manual `UPDATE`).
Costs: Kafka Connect to run and upgrade, a replication slot that pins WAL if the connector
stops (a disk-full risk to the primary), and more to explain for a small team. The outbox
table already uses Debezium's outbox-router column names, so switching later changes no
writer. **Revisit** when relay polling shows up in database load, or when more services need
their changes streamed.

**CDC of the tables themselves (not an outbox).** Would stream every column change of `posts`
straight into the index, but couples consumers to the table layout: renaming a column becomes
a breaking change for the search index. Explicit events keep the schema private.

**pgvector instead of Elasticsearch for semantic search.** At this project's scale (well under
10M posts) an HNSW index in PostgreSQL would serve kNN, and would remove a whole cluster.
Elasticsearch stays for now because keyword relevance (BM25, analyzers, highlighting) is
also needed and the projection is already cheap to rebuild. **Revisit** if the Elasticsearch
cluster becomes the largest operational cost; the reads already go through two functions
(`backend.SearchPostIDs`, `backend.NearestPostIDs`).

**Cassandra / ScyllaDB for messages.** The right model at very high write volume (a partition
per conversation, clustering by time). Operationally heavy for a second datastore, and
PostgreSQL with the same key design (`(conversation_id, seq)`) can be hash-partitioned by
conversation when a single table becomes too large. The access pattern does not change.

**A database per service now.** The textbook microservice boundary. It would turn every
cross-service read (followers for fan-out, "does this user exist") into an API call or a
replicated copy, plus the events to keep copies fresh, before any team or scale requires it.
Instead the boundary is kept in code: ownership per table, no cross-owner foreign keys, cross
reads in one package. Moving `follows` out later means replacing `shared/socialgraph`'s
queries with calls to the social service or a local replica fed by `follow.*` events.

## Consequences

Positive
- Races are closed by constraints and proven by concurrency tests against a real PostgreSQL.
- Every state change and its event commit atomically, including `post.liked` (no longer lost
  when Kafka is down) and `post.deleted` (the index learns about deletes).
- Search can lag or be rebuilt from scratch without users seeing deleted posts or stale counts.
- Lists page correctly (keyset) instead of silently truncating.
- Embeddings moved off the upload path and are stored, so a reindex does not pay OpenAI again.
- Point-in-time recovery, standard tooling (`pg_dump`, replicas, Cloud SQL / RDS).

Negative, and what we do about them
- **One more stateful system** to run (backups, failover, upgrades). Managed Postgres in the
  cloud design ([CLOUD.md](../CLOUD.md)).
- **Hot rows:** a viral post's counter and a busy conversation's sequence are single rows. The
  counter stays write-behind (Redis, flushed in batches) with a reconciler; a conversation's
  lock is held only for the insert (a direct conversation has two writers).
- **Shared database coupling** between services, see alternative above.
- **A migration** of existing data: `cmd/backfill`, idempotent and resumable, then
  `cmd/reindex`; procedure in [MIGRATION.md](../MIGRATION.md).
- **Search is eventually consistent:** a new post becomes searchable after the fast-path
  publish, the indexer and an Elasticsearch refresh (lag not measured yet; minutes or more if
  Kafka or Elasticsearch is down). Acceptable for search; everything a user edits is read back
  from PostgreSQL.
