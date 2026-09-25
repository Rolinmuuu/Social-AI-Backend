# SocialAI — Distributed AI-Driven Social Network

A microservices social platform in **Go** with **Kafka**, **Redis**, **Elasticsearch** and **Docker**: AI-generated images (DALL·E 3), semantic search, hybrid push/pull home feeds, JWT auth and a Prometheus/ELK observability stack.

Frontend and live demo: [Social-AI-Frontend](https://github.com/Rolinmuuu/Social-AI-Frontend).

**Design docs:** [Architecture](docs/ARCHITECTURE.md) (write paths, consistency, concurrency) ·
[Bottlenecks & evidence](docs/SCALING.md) (what saturates first, the test behind each claim, load test) ·
[Cloud design](docs/CLOUD.md) (GCP target architecture, failure modes, delivery pipeline).

| Concern | How it is handled | Code |
|---|---|---|
| Dual write (ES + Kafka) | transactional outbox inside the post document + relay with backoff | `shared/outbox` |
| Client retries of paid/slow POSTs | `Idempotency-Key` with atomic claim, replay, 409/422 | `shared/idempotency` |
| Fan-out cost, celebrity accounts | pipelined batches, push/pull switch, idempotent `ZADD` | `shared/feedplan` |
| Hot post (many likes) | atomic `SADD` + `op_type=create`; write-behind counter | `shared/counter` |
| Event processing | at-least-once, per-key order, parallel workers, in-order commits, DLQ | `shared/consumer` |
| Cache stampede | single-flight + TTL jitter | `shared/cache` |

---

## Architecture Overview

```
                        ┌─────────────────────────────────────────────────────┐
                        │                  Client (React SPA)                  │
                        └───────────────────────┬─────────────────────────────┘
                                                │ HTTP :80
                        ┌───────────────────────▼─────────────────────────────┐
                        │             Nginx  (API Gateway + Rate Limit)        │
                        └──┬──────────┬──────────┬────────────┬───────────────┘
                           │          │          │            │
                     :8081 │    :8082 │    :8083 │      :8084 │
              ┌────────────▼┐  ┌──────▼──────┐  ┌─────▼────┐  ┌──▼──────────┐
              │ auth-service│  │post-service │  │  social  │  │   message   │
              │  signup     │  │  upload     │  │  follow  │  │   send      │
              │  signin     │  │  search     │  │followers │  │   history   │
              └─────────────┘  │  delete     │  └──────────┘  └─────────────┘
                               │  like/share │
                               │  comment    │
                               │  AI-generate│
                               └──────┬──────┘
                                      │ publish "post.created"
                               ┌──────▼──────┐
                               │    Kafka    │
                               └──────┬──────┘
                                      │ consume
                               ┌──────▼──────────────────────────────────────┐
                               │              feed-worker                     │
                               │  fan-out new posts → followers' Redis lists  │
                               └─────────────────────────────────────────────┘

  Shared Infrastructure
  ├── Elasticsearch   — users, posts, likes, shares, comments, follows, messages
  ├── Redis           — post cache, like dedup sets, home feed lists (fan-out)
  ├── GCS             — media storage (images & videos)
  └── ELK + Prometheus/Grafana — logging & metrics
```

---

## Tech Stack

| Layer | Technology |
|---|---|
| Language | Go 1.25 |
| API Gateway | Nginx (rate limiting, reverse proxy) |
| Authentication | JWT (HS256) via `auth0/go-jwt-middleware` |
| Primary Storage | Elasticsearch 8.13 |
| Cache & Feed Store | Redis 7 |
| Message Queue | Apache Kafka (Confluent 7.7) |
| Media Storage | Google Cloud Storage |
| AI Integration | OpenAI DALL-E 3 |
| Observability | Prometheus + Grafana + ELK (Elasticsearch + Logstash + Kibana) |
| Containerisation | Docker + Docker Compose |
| CI/CD | GitHub Actions |

---

## Services

### `auth` — Authentication Service (`:8081`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/signup` | No | Register a new user (bcrypt password hashing) |
| POST | `/signin` | No | Login and receive a JWT token |
| GET | `/health` | No | Health check |
| GET | `/metrics` | No | Prometheus metrics |

### `post` — Post Service (`:8082`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/upload` | JWT | Upload a post with media file (image/video → GCS) |
| GET | `/search` | JWT | Search posts by `user_id` or `keywords` |
| DELETE | `/post/{id}` | JWT | Soft-delete a post (async GCS cleanup) |
| POST | `/post/{id}/like` | JWT | Like a post (Redis dedup + ES) |
| POST | `/post/{id}/share` | JWT | Share a post to a platform |
| POST | `/post/{id}/comment` | JWT | Add a comment or reply to a post |
| POST | `/post/generate-image-from-openai` | JWT | Generate image with DALL-E 3 and auto-publish as a post |
| GET | `/feed?limit=&cursor=` | JWT | Home feed: pushed posts merged with followed high-follower accounts, cursor paging |

`POST /upload` and `POST /post/generate-image-from-openai` accept an `Idempotency-Key` header: a retried request replays the first response instead of creating a second post.
| GET | `/health` | No | Health check |
| GET | `/metrics` | No | Prometheus metrics |

### `social` — Social Graph Service (`:8083`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/follow` | JWT | Follow a user |
| DELETE | `/follow` | JWT | Unfollow a user |
| GET | `/follow/followers` | JWT | List users who follow me |
| GET | `/follow/following` | JWT | List users I follow |
| GET | `/health` | No | Health check |
| GET | `/metrics` | No | Prometheus metrics |

### `message` — Messaging Service (`:8084`)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/message` | JWT | Send a direct message |
| GET | `/message?with_user_id={id}` | JWT | Retrieve conversation history |
| GET | `/health` | No | Health check |
| GET | `/metrics` | No | Prometheus metrics |

### `feed-worker` — Kafka Consumer (background worker)

Consumes `post.created`. Authors with up to 5,000 followers are **pushed**: the post id is added to each follower's `feed:home:{userId}` sorted set in pipelined batches of 500 (one Redis round trip per batch, 4 in flight). Authors above the threshold are **pulled**: they are recorded in `feed:celebrities` and `GET /feed` merges their recent posts at read time. Events are processed by 8 keyed workers per instance (one author's events stay in order), offsets are committed only when processed, and poison events go to `post.created.dlq`. Metrics on `:9101/metrics`.

### `notification-worker`

Consumes `post.liked` with the same consumer; notification ids are deterministic, so a redelivered event does not notify twice. Metrics on `:9102/metrics`.

---

## Key Design Decisions

Details, diagrams and trade-offs: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

- **Transactional outbox.** A post and its `post.created` event are one Elasticsearch document write. Publishing is attempted inline; if Kafka is down the upload still returns 201 and a relay retries with exponential backoff, parking an event as `dead` after 10 attempts. Delivery is at-least-once; consumers are idempotent.
- **Idempotency keys** on upload and image generation: `SET NX` claim, stored response replayed for 24 h, `409` while in progress, `422` if the key is reused for another request. The work and its bookkeeping are detached from the HTTP connection, so a client that timed out and retries gets the first result.
- **Hybrid fan-out** (see above) replaces a per-follower loop that made three sequential Redis calls per follower, duplicated entries on redelivery, and silently skipped followers beyond the first 10,000.
- **Likes.** `SADD` (returns 1 only for the first caller) plus `op_type=create` on the like document make "check and write" one atomic step, so concurrent double taps count once. `like_count` / `shared_count` are write-behind: Redis `INCRBY`, flushed to Elasticsearch every 2 s as one update per post (per instance).
- **Reliable consumer.** Retries with backoff, dead-letter topic, commits only the highest contiguous processed offset, bounded queues for backpressure. The producer partitions by author id (`Hash` balancer) so per-author order holds end to end.
- **Cache stampede protection.** Single-flight on cache misses and ±20 % TTL jitter.
- **Media cleanup (compensation).** Deleting a post is a soft delete; a background loop removes the GCS object and retries up to 5 times. If the ES write fails during upload, the uploaded object is deleted.
- **Graceful shutdown** on SIGTERM for the API (drain requests, wait for background loops, final counter flush) and workers (finish in-flight messages, commit their offsets).
- **Known gaps** are listed at the end of [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#8-known-gaps); nothing in this README is a measured latency or throughput figure yet.

### Redis keys

| Key Pattern | Type | Purpose | TTL |
|---|---|---|---|
| `user_feed:{userId}` | String (JSON) | Cache of a user's own posts | 10 s ± 20 % |
| `like_set:{postId}` | Set | Atomic like de-duplication | none |
| `feed:home:{userId}` | Sorted set (post id → created_at) | Materialised home feed, newest 500 | 7 days |
| `feed:celebrities` | Set | Authors served by pull | none |
| `cnt:{field}:{postId}`, `cnt:dirty` | String, Set | Unflushed counter deltas | until flushed |
| `idem:{scope}:{userId}:{key}` | String (JSON) | Idempotency claim / stored response | 2 min / 24 h |

### JWT Authentication

All protected routes validate a HS256 JWT signed with `JWT_SECRET`. The `auth` service issues tokens; the other services validate them independently, with no shared session state.

### Rate Limiting

Nginx enforces `100 req/s` per IP with a burst of 200 at the gateway level; the services also apply a per-IP token bucket.

---

## Project Structure

```
Backend/
├── services/
│   ├── auth/           # Authentication: signup, signin, JWT issuance
│   ├── post/           # Posts: upload, search, like, share, comment, AI-generate
│   ├── social/         # Follow graph: follow, unfollow, followers, following
│   ├── message/        # Direct messaging: send, history
│   └── feed/           # Kafka consumer: fan-out feed materialisation worker
│       └── worker/
├── shared/
│   ├── backend/        # ES, Redis, GCS client implementations + interfaces
│   ├── constants/      # Environment-aware constants (ES, Redis, Kafka, etc.)
│   ├── kafka/          # KafkaProducer + KafkaConsumer wrappers
│   ├── logger/         # Zap structured logger with optional Logstash TCP sink
│   ├── middleware/      # Prometheus metrics + request logging middleware
│   ├── model/          # Shared data models (Post, User, Follow, …) + Kafka event DTOs
│   └── utils/          # JWT extraction helpers, cache key helpers
├── nginx/nginx.conf     # Reverse proxy + rate limiting config
├── logstash/pipeline/   # Logstash pipeline config
├── prometheus.yml       # Prometheus scrape config
├── docker-compose.yml   # Full-stack orchestration
└── .github/workflows/   # CI (lint + build + test) and CD pipelines
```

---

## Getting Started

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) & Docker Compose v2
- A Google Cloud project with a GCS bucket and Application Default Credentials
- An OpenAI API key (for DALL-E 3 image generation)

### Environment Variables

Create a `.env` file in the `Backend/` directory:

```env
JWT_SECRET=your-strong-secret-here
ES_PASSWORD=
OPENAI_API_KEY=sk-...
GCS_BUCKET=your-bucket-name
```

### Run with Docker Compose

```bash
cd Backend
docker compose up --build
```

| Service | URL |
|---|---|
| API Gateway (Nginx) | http://localhost:80 |
| Elasticsearch | http://localhost:9200 |
| Kibana (logs) | http://localhost:5601 |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 (admin / admin) |
| Kafka | localhost:9092 |

### Run Tests

```bash
# Unit tests, with the race detector (as in CI)
go test -race ./...

# Elasticsearch integration tests (requires a running ES instance)
go test -tags=integration ./shared/backend/...
```

---

## CI/CD

GitHub Actions pipelines are defined in `.github/workflows/`:

- **`ci.yml`** — `gofmt`, `go build`, `go vet` and `go test -race` on every push and pull request.
- **`cd.yml`** — manual deploy (`workflow_dispatch`) once GCP secrets are configured. Target architecture: [docs/CLOUD.md](docs/CLOUD.md).

### Load test

`loadtest/k6-social.js` (browse, hot-post likes, idempotent uploads): see [docs/SCALING.md](docs/SCALING.md#load-test).

---

## Observability

| Tool | Purpose | URL |
|---|---|---|
| Prometheus | Scrapes `/metrics` from all four services | `:9090` |
| Grafana | Dashboards over Prometheus data | `:3000` |
| Logstash | Receives structured JSON logs via TCP `:5000` | — |
| Elasticsearch-logs | Stores application logs (separate from data ES) | `:9201` |
| Kibana | Log search and visualisation | `:5601` |
