# Cloud architecture (target design for GCP)

> Status: design. The repository runs locally with `docker compose`; nothing here is
> deployed. GCP is chosen because the code already uses Cloud Storage and the CD workflow
> targets GCP.

```mermaid
flowchart TB
  U[Clients] --> LB[Global HTTPS Load Balancer<br/>+ Cloud Armor: WAF, per-IP rate limits]
  LB --> CDN[Cloud CDN] --> GCS[(Cloud Storage<br/>media, private + signed URLs)]
  LB --> CR

  subgraph CR[Cloud Run — stateless HTTP, autoscaled on concurrency]
    AUTH[auth] 
    POST[post]
    SOC[social]
    MSG[message]
  end

  subgraph GKE[GKE Autopilot — always-on workers, KEDA scales on consumer lag]
    JOBS[post-jobs<br/>outbox relay, counter flush, media cleanup]
    FEED[feed-worker]
    NOTI[notification-worker]
  end

  CR -->|Direct VPC egress| MR[(Memorystore Redis<br/>Standard tier: primary + replica)]
  CR --> ESC[(Elasticsearch on Elastic Cloud<br/>3 nodes, 3 zones)]
  CR --> KF{{Managed Kafka<br/>GCP Managed Service for Apache Kafka or Confluent Cloud}}
  JOBS --> ESC & KF & MR
  KF --> FEED & NOTI
  FEED --> MR
  NOTI --> ESC

  SM[Secret Manager] -.-> CR & GKE
  OBS[Managed Prometheus + Cloud Logging + Cloud Trace] -.-> CR & GKE
```

## Why this split

| Component | Runs on | Reason |
|---|---|---|
| auth, post, social, message | Cloud Run | Stateless request/response; scales to zero off-peak, per-request autoscaling on bursts. `post` keeps `min-instances ≥ 1` to avoid cold starts on uploads. |
| outbox relay, counter flusher, media cleanup | `post-jobs` deployment on GKE (1–2 replicas) | They are loops, not requests. On Cloud Run with request-based billing the CPU is throttled between requests, so the loops would stall. Running them as their own deployment also lets the API scale without multiplying pollers. |
| feed-worker, notification-worker | GKE Autopilot + KEDA Kafka scaler | Pull-based consumers need to run continuously. KEDA scales replicas on consumer-group lag, capped at the partition count (extra consumers in a group would sit idle). |
| Kafka | Managed (GCP Managed Service for Apache Kafka, or Confluent Cloud) | Replication factor 3 across zones, `acks=all` (the producer already requires it). `post.created`: 12 partitions keyed by author id. |
| Redis | Memorystore, Standard tier | Automatic failover. Holds feeds (rebuildable), idempotency keys (24 h), and unflushed counter deltas — enable persistence (RDB snapshots) so a failover loses at most seconds of deltas. |
| Elasticsearch | Elastic Cloud on GCP, 3 zones | System of record and vector search; 1 replica per shard so a zone loss keeps every shard available. |
| Media | Cloud Storage + Cloud CDN | Objects stay private; the API issues signed URLs, the CDN serves repeated reads. |

## Scaling knobs and what they protect

| Knob | Protects | Starting value |
|---|---|---|
| Cloud Run `max-instances` for post | Elasticsearch from connection storms | sized so instances × ES client pool ≤ ES capacity |
| Cloud Run concurrency | tail latency per instance | 40–80 requests/instance |
| Kafka partitions | consumer parallelism ceiling | 12 (feed-worker replicas ≤ 12, 8 keyed workers each) |
| `feedplan.Policy.CelebrityThreshold` | fan-out cost of a single post | 5,000 followers |
| Counter flush interval | ES write rate for hot posts | 2 s |
| Cloud Armor rate rules | abuse and retry storms at the edge | per-IP limits mirroring the Nginx `limit_req` config |

## Failure modes

| Failure | Behaviour |
|---|---|
| Kafka unavailable | Uploads and likes still succeed. `post.created` waits in the outbox and is published when Kafka returns; `post.liked` notifications are dropped (best-effort by design). |
| Redis failover | Caches refill from ES; like de-duplication falls back to the ES `op_type=create` check; idempotency is skipped for requests in the failover window (logged); counter deltas not yet flushed can be lost; the like documents in ES stay correct, and a recount job (not written yet) would restore the counts. |
| A feed-worker crashes mid-batch | Its offsets were not committed; another instance re-processes. `ZADD` makes the repeat harmless. |
| Poison event | Moved to `post.created.dlq` after 5 attempts; alert on `consumer_dead_letters_total > 0`. |
| Zone outage | Cloud Run, GKE Autopilot, Memorystore Standard, Elastic Cloud and managed Kafka are all multi-zone within the region. |
| Region outage | Not covered by this design (single region). Next step would be ES cross-cluster replication and a standby region. |

## Delivery pipeline

1. GitHub Actions CI: `gofmt`, `go vet`, `go build`, `go test -race`.
2. On `main`: build one image per service, push to Artifact Registry, tagged with the commit SHA.
3. Authenticate with **Workload Identity Federation** (no long-lived JSON keys in GitHub secrets).
4. Cloud Run: deploy a new revision with no traffic, run a smoke test against its tagged URL,
   then shift traffic 10 % → 50 % → 100 %; roll back by moving traffic to the previous revision.
5. GKE workers: rolling update. SIGTERM triggers the graceful shutdown already in the code
   (in-flight messages finish; offsets of finished messages are committed; counters flushed).

## Observability

- Metrics: the services already expose Prometheus metrics (`/metrics`); Google Cloud Managed
  Service for Prometheus scrapes them without running Prometheus. Key alerts: p95 latency per
  route, `outbox_publish_lag_seconds` p95 > 30 s (with the default settings an outage shows up
  as growing lag, since records are never parked), `consumer_dead_letters_total`,
  consumer-group lag.
- Logs: structured JSON (zap) to Cloud Logging instead of the self-hosted ELK stack.
- Tracing (next step): OpenTelemetry with the post id propagated in Kafka headers, so one trace
  covers upload → outbox → feed fan-out.
