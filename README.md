# SocialAI — Distributed AI-Driven Social Network

A high-concurrency, microservices-based social platform built with **Go**, **Kafka**, **Redis**, **Elasticsearch**, and **Docker**. Features AI-generated media via OpenAI DALL-E 3, asynchronous feed materialization, JWT authentication, and a full observability stack.

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
| Language | Go 1.21 |
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

Consumes `post.created` events from Kafka. For each new post, queries the poster's followers from Elasticsearch and fan-outs the post into each follower's `home_feed:{userId}` Redis list (capped at 100 entries, 24-hour TTL). Decouples the expensive fan-out from the write path, keeping `POST /upload` fast under spiky traffic.

---

## Key Design Decisions

### Asynchronous Feed Materialization (Fan-out on Write)

When a user creates a post, `post-service` publishes a `post.created` Kafka event immediately after writing to Elasticsearch, then returns `201 Created`. The `feed-worker` asynchronously distributes the post to all followers' Redis lists. This ensures write latency stays low regardless of follower count.

```
POST /upload  →  ES + GCS  →  Kafka publish  →  201 (fast)
                                    ↓ async
                            feed-worker consumes
                                    ↓
                      Redis LPUSH home_feed:{followerId}  (for each follower)
```

### Redis Caching Strategy

| Key Pattern | Type | Purpose | TTL |
|---|---|---|---|
| `user_feed:{userId}` | String (JSON) | Cache of a user's own posts | 10s |
| `like_set:{postId}` | Set | Fast dedup check before ES query | No expiry |
| `home_feed:{userId}` | List | Materialised follower feed (max 100 items) | 24h |

### JWT Authentication

All protected routes validate a HS256 JWT signed with `JWT_SECRET`. The token contains `user_id` and `exp` claims. The `auth` service issues tokens; all other services validate them independently — no shared session state.

### Media Cleanup (Saga Pattern)

Deleting a post performs a soft-delete in Elasticsearch (sets `deleted=true`, `cleanup_status=pending`). A background goroutine in `post-service` polls every 10 seconds and removes orphaned GCS objects, updating `cleanup_status` to `completed` or retrying up to 5 times before marking `failed`.

### Rate Limiting

Nginx enforces `100 req/s` per IP with a burst of 200 at the gateway level, providing protection against traffic spikes before requests reach any Go service.

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
# Unit + integration tests (default build tags)
go test ./...

# Elasticsearch integration tests (requires a running ES instance)
go test -tags=integration ./shared/backend/...
```

---

## CI/CD

GitHub Actions pipelines are defined in `.github/workflows/`:

- **`ci.yml`** — runs `gofmt`, `go build ./...`, and `go test ./...` on every push and pull request.
- **`cd.yml`** — builds and deploys services on merge to `main`.

---

## Observability

| Tool | Purpose | URL |
|---|---|---|
| Prometheus | Scrapes `/metrics` from all four services | `:9090` |
| Grafana | Dashboards over Prometheus data | `:3000` |
| Logstash | Receives structured JSON logs via TCP `:5000` | — |
| Elasticsearch-logs | Stores application logs (separate from data ES) | `:9201` |
| Kibana | Log search and visualisation | `:5601` |
