# SocialAI — Distributed AI-Driven Social Network

A microservices social platform in **Go** with **PostgreSQL**, **Kafka**, **Redis**, **Elasticsearch** and **Docker**: AI-generated images (DALL·E 3), keyword and semantic search, hybrid push/pull home feeds, direct messages, JWT auth and a Prometheus/ELK observability stack.

Frontend and live demo: [Social-AI-Frontend](https://github.com/Rolinmuuu/Social-AI-Frontend).

**Design docs:** [Architecture](docs/ARCHITECTURE.md) (data ownership, write paths, consistency, concurrency) ·
[ADR 0001: PostgreSQL as system of record](docs/adr/0001-postgres-system-of-record.md) (why, alternatives, costs) ·
[Migration runbook](docs/MIGRATION.md) (moving existing Elasticsearch data) ·
[Bottlenecks & evidence](docs/SCALING.md) (what saturates first, the test behind each claim, load test) ·
[Cloud design](docs/CLOUD.md) (GCP target architecture, failure modes, delivery pipeline).

| Concern | How it is handled | Code |
|---|---|---|
| Source of truth | PostgreSQL; uniqueness and integrity are constraints, not read-then-write code | `migrations/`, `shared/db` |
| Dual write (DB + Kafka) | transactional outbox table, relays claim rows with `FOR UPDATE SKIP LOCKED` | `shared/outbox` |
| Search | Elasticsearch is a projection rebuilt from PostgreSQL; versioned writes, tombstones, hits re-read from the DB | `services/indexer`, `shared/backend/search.go` |
| Client retries of paid/slow POSTs | `Idempotency-Key` with atomic claim, replay, 409/422 | `shared/idempotency` |
| Fan-out cost, celebrity accounts | pipelined batches, push/pull switch, idempotent `ZADD` | `shared/feedplan` |
| Hot post (many likes) | primary-key de-duplication; write-behind counter with a reconciler | `shared/counter`, `services/post/service/reconcile.go` |
| Message ordering | per-conversation sequence assigned under a row lock; keyset paging | `services/message` |
| Event processing | at-least-once, per-key order, parallel workers, in-order commits, DLQ | `shared/consumer` |
| Cache stampede | single-flight + TTL jitter | `shared/cache` |

---

## Architecture Overview

```
 React SPA ──HTTP──► Nginx (gateway, rate limit)
                       ├──► auth     ─┐
                       ├──► social   ─┤  read/write          ┌───────────────────────┐
                       ├──► message  ─┼────────────────────► │      PostgreSQL       │
                       └──► post     ─┘  row + outbox row,   │   system of record    │
                             │           one transaction     └───────────┬───────────┘
                             │                                           │ outbox relay
                             │ search: ranked ids                        ▼
                             ▼                                        Kafka ──┐
                      Elasticsearch  ◄── versioned writes ── search-indexer ◄─────┤
                    (post projection)                                             │
                                         Redis feeds ◄── feed-worker ◄────────────┤
                                                   notification-worker ◄──────────┘
```

Search hits are re-read from PostgreSQL before they are returned; Elasticsearch and Redis can both be rebuilt from it.

---

## Tech Stack

| Layer | Technology |
|---|---|
| Language | Go 1.25 |
| API Gateway | Nginx (rate limiting, reverse proxy) |
| Authentication | JWT (HS256) via `auth0/go-jwt-middleware` |
| System of record | PostgreSQL 16 (`pgx/v5`), versioned SQL migrations |
| Search | Elasticsearch 8.13 (BM25 + dense-vector kNN), fed from PostgreSQL |
| Cache, feeds, counters | Redis 7 |
| Message Queue | Apache Kafka (Confluent 7.7) |
| Media Storage | Google Cloud Storage |
| AI Integration | OpenAI DALL-E 3, `text-embedding-3-small` |
| Observability | Prometheus + Grafana + ELK (Elasticsearch + Logstash + Kibana) |
| Containerisation | Docker + Docker Compose |
| CI/CD | GitHub Actions (tests run against a real PostgreSQL) |

---

## Services

List endpoints are keyset-paged: pass the `next_cursor` of one page as `cursor` to get the next; it is omitted on the last page.

### `auth` — Authentication Service (`:8081`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/signup` | No | Register a new user (bcrypt password hashing; `409` if the id is taken) |
| POST | `/signin` | No | Login and receive a JWT token |
| GET | `/health` | No | Health check |
| GET | `/metrics` | No | Prometheus metrics |

### `post` — Post Service (`:8082`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/upload` | JWT | Upload a post with media file (image/video → GCS) |
| GET | `/search` | JWT | `user_id=` a user's posts · `keywords=` keyword search · `keywords=&mode=semantic` vector search · no params: newest posts |
| GET | `/feed?limit=&cursor=` | JWT | Home feed: pushed posts merged with followed high-follower accounts |
| DELETE | `/post/{id}` | JWT | Soft-delete your own post (async GCS cleanup) |
| POST | `/post/{id}/like` | JWT | Like a post (`409` if already liked) |
| DELETE | `/post/{id}/like` | JWT | Remove your like |
| POST | `/post/{id}/share` | JWT | Share a post to a platform |
| POST | `/post/{id}/comment` | JWT | Add a comment, or a reply with `parent_comment_id` |
| GET | `/post/{id}/comments?limit=&cursor=` | JWT | Comments on a post, oldest first |
| POST | `/post/generate-image-from-openai` | JWT | Generate image with DALL-E 3 and publish it as a post |
| GET | `/health`, `/metrics` | No | Health check, Prometheus metrics |

`POST /upload` and `POST /post/generate-image-from-openai` accept an `Idempotency-Key` header: a retried request replays the first response instead of creating a second post.

### `social` — Social Graph Service (`:8083`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/follow` | JWT | Follow a user (`404` unknown user, `409` already following) |
| DELETE | `/follow` | JWT | Unfollow a user |
| GET | `/follow/followers?user_id=&limit=&cursor=` | JWT | Followers of a user (default: me), newest first |
| GET | `/follow/following?user_id=&limit=&cursor=` | JWT | Accounts a user follows, newest first |
| GET | `/health`, `/metrics` | No | Health check, Prometheus metrics |

### `message` — Messaging Service (`:8084`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/message` | JWT | Send a direct message; `Idempotency-Key` makes retries safe (`200` + `Idempotent-Replayed` on replay) |
| GET | `/message?with_user_id=&limit=` | JWT | Newest messages of the conversation, oldest first; `next_before_seq` pages back |
| GET | `/message?with_user_id=&before_seq=N` | JWT | Older history |
| GET | `/message?with_user_id=&after_seq=N` | JWT | Messages since `N` (catching up / polling) |
| GET | `/conversations?limit=&cursor=` | JWT | Inbox: conversations by recent activity, last message, unread count |
| POST | `/conversations/read` | JWT | `{"with_user_id", "seq"}`: mark read up to `seq` |
| GET | `/health`, `/metrics` | No | Health check, Prometheus metrics |

### `feed-worker` — Kafka Consumer

Consumes `post.created`. Authors with up to 5,000 followers are **pushed**: the post id is added to each follower's `feed:home:{userId}` sorted set in pipelined batches of 500 (one Redis round trip per batch, 4 in flight). Authors above the threshold are **pulled**: they are recorded in `feed:celebrities` and `GET /feed` merges their recent posts at read time. Followers are read from PostgreSQL; the follower count stops at 5,001. Metrics on `:9101/metrics`.

### `search-indexer` — Kafka Consumer

Consumes `post.created` and `post.deleted`, reads the post's current row and writes it to the `posts` index with the row version as an external version, so duplicates and out-of-order events converge. Computes embeddings (off the upload path) and stores them in PostgreSQL. Metrics on `:9103/metrics`.

### `notification-worker` — Kafka Consumer

Consumes `post.liked`; notification ids are deterministic and inserts are `ON CONFLICT DO NOTHING`, so a redelivered event does not notify twice. Metrics on `:9102/metrics`.

### One-shot commands (`cmd/`)

`migrate` (schema), `reindex` (rebuild the search index, optionally into a new index behind the alias), `backfill` (one-time copy of legacy Elasticsearch data, see [MIGRATION.md](docs/MIGRATION.md)).

---

## Key Design Decisions

Details, diagrams and trade-offs: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

- **PostgreSQL is the system of record; Elasticsearch is a projection.** Races that the old check-then-write code allowed (duplicate follows, a second sign-up overwriting a password, double likes) are now primary-key conflicts. Search returns ranked ids and rows are re-read from PostgreSQL, so a lagging index never shows a deleted post. The reasoning and the alternatives (Debezium, pgvector, a database per service) are in [ADR 0001](docs/adr/0001-postgres-system-of-record.md).
- **Transactional outbox.** A change and its event are inserted in one transaction. Publishing is attempted inline; if Kafka is down the request still succeeds and relays (one per instance, `FOR UPDATE SKIP LOCKED` claims with a lease) retry with exponential backoff. Delivery is at-least-once; consumers are idempotent.
- **Versioned search projection.** The indexer writes with `version_type=external` from `posts.version`, keeps tombstones for deleted posts, and `cmd/reindex` rebuilds the index behind an alias.
- **Messages** are clustered by `(conversation_id, seq)`. `seq` comes from a per-conversation counter under a row lock, so it is gap-free and in commit order — which paging by `created_at` would not be.
- **Idempotency keys** on upload and image generation: `SET NX` claim, stored response replayed for 24 h, `409` while in progress, `422` if the key is reused for another request.
- **Hybrid fan-out** with pipelined batches and a push/pull switch for high-follower accounts.
- **Likes** are one `INSERT … ON CONFLICT DO NOTHING`; `like_count` / `shared_count` are write-behind (Redis `INCRBY`, flushed every 2 s as one `UPDATE` per post) and a reconciler repairs drift from the rows.
- **Reliable consumer.** Retries with backoff, dead-letter topic, commits only the highest contiguous processed offset, bounded queues for backpressure.
- **Keyset pagination everywhere** (feed, followers, comments, messages, inbox) — the old searches silently stopped at 10 results.
- **Graceful shutdown** on SIGTERM for the API (drain requests, wait for background loops, final counter flush) and workers (finish in-flight messages, commit their offsets).
- **Known gaps** are listed at the end of [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#10-known-gaps); nothing in this README is a measured latency or throughput figure yet.

### Redis keys

| Key Pattern | Type | Purpose | TTL |
|---|---|---|---|
| `user_feed:{userId}` | String (JSON) | Cache of a user's own posts | 10 s ± 20 % |
| `feed:home:{userId}` | Sorted set (post id → created_at) | Materialised home feed, newest 500 | 7 days |
| `feed:celebrities` | Set | Authors served by pull | none |
| `cnt:{field}:{postId}`, `cnt:dirty` | String, Set | Unflushed counter deltas | until flushed |
| `idem:{scope}:{userId}:{key}` | String (JSON) | Idempotency claim / stored response | 2 min / 24 h |

Everything in Redis can be lost without losing data: feeds, caches and counters are rebuilt or repaired from PostgreSQL.

### JWT Authentication

All protected routes validate a HS256 JWT signed with `JWT_SECRET`. The `auth` service issues tokens; the other services validate them independently, with no shared session state.

### Rate Limiting

Nginx enforces `100 req/s` per IP with a burst of 200 at the gateway level; the services also apply a per-IP token bucket.

---

## Project Structure

```
Backend/
├── migrations/          # Versioned SQL schema (embedded, applied by cmd/migrate)
├── cmd/
│   ├── migrate/         # Apply migrations (one-shot job)
│   ├── reindex/         # Rebuild the post search index from PostgreSQL
│   └── backfill/        # One-time copy of legacy Elasticsearch data into PostgreSQL
├── services/
│   ├── auth/            # Sign-up, sign-in, JWT issuance            (owns users)
│   ├── post/            # Posts, likes, shares, comments, search    (owns posts, outbox, …)
│   ├── social/          # Follow graph                              (owns follows)
│   ├── message/         # Direct messages, inbox                    (owns conversations, messages)
│   ├── feed/            # Kafka consumer: fan-out into Redis feeds
│   ├── indexer/         # Kafka consumer: PostgreSQL → Elasticsearch projection
│   └── notification/    # Kafka consumer: like notifications        (owns notifications)
├── shared/
│   ├── db/              # pgx pool, transactions, migrator; dbtest = a real schema per test
│   ├── outbox/          # Relay + PostgreSQL store (Enqueue, SKIP LOCKED claims, prune)
│   ├── socialgraph/     # Read-only queries over users/follows used by other services
│   ├── backend/         # Elasticsearch (search projection), Redis, GCS, OpenAI clients
│   ├── consumer/        # Reliable Kafka consumption (ordering, retries, DLQ, commits)
│   ├── feedplan/        # Push/pull decision, batched fan-out, feed merge
│   ├── counter/         # Write-behind counters
│   ├── idempotency/     # Idempotency-Key store
│   ├── pagecursor/      # Opaque keyset cursors
│   └── …                # cache, kafka, logger, metrics, middleware, model, utils, testutil
├── docs/                # Architecture, ADRs, migration runbook, scaling, cloud design
├── nginx/nginx.conf     # Reverse proxy + rate limiting config
├── docker-compose.yml   # Full-stack orchestration (postgres → migrate → services)
└── .github/workflows/   # CI (fmt, vet, race tests against PostgreSQL, compose check) and CD
```

---

## Getting Started

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) & Docker Compose v2
- Nothing else for local use: media goes to a GCS emulator (fake-gcs-server) in compose. For a real bucket, see the environment variables below.
- An OpenAI API key (image generation and embeddings; without it search is keyword-only)

### Environment Variables

Create a `.env` file in the `Backend/` directory:

```env
JWT_SECRET=your-strong-secret-here
POSTGRES_PASSWORD=choose-one        # optional, defaults to "socialai" for local use
ES_PASSWORD=
OPENAI_API_KEY=sk-...
# Real Cloud Storage instead of the local emulator (also mount Application Default Credentials):
# STORAGE_EMULATOR_HOST=
# GCS_PUBLIC_BASE_URL=
# GCS_BUCKET=your-bucket-name
```

### Run with Docker Compose

```bash
cd Backend
docker compose up --build
```

`postgres` starts first, the one-shot `migrate` service applies the schema, and only then do the services start.

| Service | URL |
|---|---|
| API Gateway (Nginx) | http://localhost:80 |
| PostgreSQL | localhost:5432 (`socialai` / `$POSTGRES_PASSWORD`) |
| Elasticsearch | http://localhost:9200 |
| Kibana (logs) | http://localhost:5601 |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 (admin / admin) |
| Kafka | localhost:9092 |
| GCS emulator (media) | http://localhost:4443/socialai-media/&lt;object&gt; |

### Run Tests

Tests that touch SQL run against a real PostgreSQL: each test gets a fresh, migrated schema that is dropped afterwards, so tests run in parallel. Without `TEST_DATABASE_URL` they are skipped (CI sets `REQUIRE_DB=1`, which turns a missing database into a failure).

```bash
docker compose up -d postgres
export TEST_DATABASE_URL="postgres://socialai:socialai@localhost:5432/socialai?sslmode=disable"
go test -race ./...

# Elasticsearch, Redis and Kafka behaviour the in-memory doubles imitate (needs the running stack)
ES_URL=http://localhost:9200 REDIS_ADDRESS=localhost:6379 KAFKA_BROKERS=localhost:9092 \
  go test -tags=integration ./shared/backend/ ./shared/kafka/

# End to end: the whole compose stack, one user journey through the gateway
JWT_SECRET=dev OPENAI_API_KEY=dummy docker compose up -d --build
go test -tags=e2e -count=1 -v ./e2e/
```

Kafka on `localhost:9092` needs the broker to advertise an address your machine can reach; compose advertises `kafka:9092`, so run the Kafka tests from a container on the compose network or against a broker that advertises `localhost`.

---

## CI/CD

GitHub Actions pipelines are defined in `.github/workflows/`:

- **`ci.yml`**, on pull requests and pushes to `main` / `feature/**`:
  - `test` — `gofmt`, `go build`, `go vet`, `go test -race` against a PostgreSQL service container, `docker compose config`.
  - `integration` — the integration-tagged tests against real Elasticsearch 8.13, Kafka and Redis.
  - `e2e` — `docker compose up` of every service, then `e2e/e2e_test.go`: sign-up, follow, idempotent upload, fan-out to the follower's feed, keyword search, like → notification, comments, idempotent messages, delete → search tombstone, outbox fully published. Fails if any container exited or restarted.
- **`cd.yml`** — manual deploy (`workflow_dispatch`) once GCP secrets are configured. Target architecture: [docs/CLOUD.md](docs/CLOUD.md).

### Load test

`loadtest/k6-social.js` (browse, hot-post likes, idempotent uploads): see [docs/SCALING.md](docs/SCALING.md#load-test).

---

## Observability

| Tool | Purpose | URL |
|---|---|---|
| Prometheus | Scrapes `/metrics` from the four services and three workers | `:9090` |
| Grafana | Dashboards over Prometheus data | `:3000` |
| Logstash | Receives structured JSON logs via TCP `:5000` | — |
| Elasticsearch-logs | Stores application logs (separate from the search cluster) | `:9201` |
| Kibana | Log search and visualisation | `:5601` |

Metrics worth alerting on: `outbox_oldest_pending_seconds` (events not reaching Kafka), `consumer_dead_letters_total`, `post_counters_repaired_total` (counter drift), and p95 latency per route.
