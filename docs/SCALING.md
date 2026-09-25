# Bottlenecks, scaling and evidence

This page lists where the system saturates first, what was changed, and how each claim is
verified. "Verified" means an automated test in this repository. The tests run against
in-memory doubles of Redis, Elasticsearch and Kafka; the Elasticsearch double evaluates the
`bool`/`term`/`terms`/`range`/`exists` queries the services build (so query logic is tested),
but the doubles do not reproduce cluster behaviour such as refresh delay or network latency.
Numbers that need a real cluster are marked **to measure** and come with the load test that
produces them. No latency or throughput figure has been measured yet.

## Where it saturates, in order

| # | Hot spot | Symptom | Change | Evidence |
|---|---|---|---|---|
| 1 | Fan-out of one post to many followers | feed-worker lag grows with follower count; 3 Redis round trips per follower | pipelined batches of 500, 4 in flight; push/pull switch above 5,000 followers | `TestFanOutBatchesAndBoundsConcurrency` (4,321 followers → 9 round trips instead of 12,963), `TestHandlePostCreated_BatchesFollowerWrites`, `TestHandlePostCreated_CelebrityIsPulledNotPushed` |
| 2 | Likes on one popular post | ES version conflicts on the post document, 500s | Redis `INCRBY` + periodic flush: one ES write per post per flush per post-service instance | `TestThousandConcurrentLikesBecomeOneWrite` (1,000 concurrent likes → 1 ES update of +1000), `TestFailedFlushKeepsTheDelta` |
| 3 | Cache expiry of a hot key | burst of identical ES queries | single-flight + TTL jitter | `TestSearchPostByUserId_ConcurrentMissesShareOneQuery` (50 concurrent misses → ≤ 2 queries), `TestConcurrentMissesQueryOnce` |
| 4 | Synchronous Kafka publish on the request path | ~1 s per request that publishes (kafka-go default batch timeout); with the broker down, the writer's default 10 retries held uploads for seconds | `BatchTimeout` 10 ms; inline publish capped at 1 s, writer retries cut to 3; failures deferred to the outbox relay | code review of `shared/kafka/producer.go` and `outbox.PublishNow`; latency **to measure** (`upload` scenario) |
| 5 | Search payload size | every hit carried a 1,536-float vector | vector excluded by source filter | code review; payload size **to measure** |
| 6 | Consumer throughput | one message at a time per instance | 8 keyed workers per instance, bounded queues, horizontal scale via consumer group (6 partitions in compose) | `TestPerKeyOrderIsKeptWhileKeysRunInParallel` |
| 7 | Home feed read | loaded the whole feed (up to 500 posts) from ES on every page | reads only the next `limit+1` ids from Redis and loads only the page's posts | `TestGetHomeFeed_PagingNeverRepeatsOrSkips`; latency **to measure** (`browse` scenario) |

## Correctness under failure and concurrency

| Guarantee | Test |
|---|---|
| Kafka outage does not fail uploads, and the event is published once the broker is back | `TestSavePost_KafkaDown_PostSavedAndEventPublishedLater`, `TestBrokerOutageDoesNotLoseEvents` |
| A 16-hour outage only delays an event; it is never given up on (default settings) | `TestLongOutageNeverParksByDefault` |
| Shutdown mid-pass does not use up a record's retry attempts | `TestCancelledContextDoesNotSpendAnAttempt` |
| With parking enabled, a poison event is parked, never dropped, and does not block others | `TestPoisonRecordIsParkedNotDropped`, `TestPoisonMessageGoesToDLQAndDoesNotBlockThePartition` |
| No offset is committed past a message that was neither handled nor dead-lettered | `TestNothingIsCommittedPastAMessageThatCouldNotBeParked`, `TestTrackerCommitsOnlyContiguousOffsets` |
| Redelivered `post.created` does not duplicate feed entries | `TestHandlePostCreated_RedeliveryIsIdempotent` |
| 50 concurrent retries with one Idempotency-Key run the handler once; later retries replay | `TestConcurrentRetriesRunTheHandlerOnce`, `TestUploadRetryWithSameKeyCreatesOnePost` |
| Client disconnects after the post is saved, then retries: one post, the retry replays | `TestUploadClientDisconnectThenRetryReplays` |
| 20 concurrent likes by one user count once | `TestLikePost_ConcurrentDoubleTapCountsOnce` |
| Like de-duplication survives losing the Redis set | `TestLikePost_DurableCheckWhenRedisWasFlushed` |
| Deleting a post does not overwrite a concurrently updated like count | `TestDeletePost_DoesNotOverwriteConcurrentCounterUpdates` |
| SIGTERM during a counter flush loses no delta; a half-recorded like is never counted twice | `TestShutdownDuringFlushLosesNothing`, `TestIncrThatCannotScheduleIsUndone` |
| Feed paging over pushed + pulled posts with tied timestamps, undated and deleted posts: every post exactly once, in order | `TestGetHomeFeed_PagingNeverRepeatsOrSkips`, `TestGetHomeFeed_MergesPushedAndCelebrityPosts`, `TestBeforeCursorPagesWithoutGapsOrRepeats` |

CI runs `go vet` and all tests with the race detector (`go test -race`).

## Back-of-envelope capacity (assumptions stated)

Assumptions: 1M users, 10% daily active, each reads the feed 10×/day and posts 0.2×/day;
average 200 followers; peak = 5× average.

| Quantity | Estimate |
|---|---|
| Posts/day | 100k DAU × 0.2 = 20k → ~0.23/s average, ~1.2/s peak |
| Feed writes (push) | 20k × 200 = 4M `ZADD`/day. Pipelines are per post, and an average post (200 followers) fits in one, so ≈ 20k round trips/day (~1.2/s at peak) — trivial for one Redis |
| Feed reads | 100k × 10 = 1M/day → ~12/s average, ~60/s peak; per page: one bounded `ZRANGE`, one ES query for followed celebrities, one terms query for at most `limit` posts |
| Feed memory | a feed exists for every follower who received a push, not only daily actives, and each push refreshes its 7-day TTL. Upper bound: 500 entries × ~100 B (36-char UUID member + score + skiplist overhead) ≈ 50 KB per feed. With ~500k users receiving a push in any week that is up to ~25 GB; in practice far less, because most feeds hold well under 500 entries. Worth measuring with `MEMORY USAGE` before sizing, and the reason `MaxFeedLen` and the TTL are knobs |
| Likes | the hot-post case matters more than the average: write-behind caps ES writes at 1 per post per 2 s per post-service instance, regardless of like rate |

The first real limits at this scale are Elasticsearch (search and kNN) and the celebrity
threshold; both are covered above. Kafka and Redis are far from saturated.

## Load test

`loadtest/k6-social.js` runs three scenarios against the gateway from `docker compose up`:
browsing (search + feed), many users liking one post, and uploads that are each retried
with the same Idempotency-Key.

```bash
docker compose up --build -d
k6 run -e BASE=http://localhost -e VUS=50 loadtest/k6-social.js
```

The gateway rate-limits each client IP to 100 r/s (burst 200). From one machine, anything
above that measures the limiter, not the services; raise the limit in `nginx/nginx.conf` for
a capacity run.

Watch in Prometheus (`:9090`) / Grafana (`:3000`) while it runs:

- `histogram_quantile(0.95, sum by (le, path) (rate(http_request_duration_seconds_bucket[1m])))` — p95 per route
- `rate(outbox_published_total[1m])`, `histogram_quantile(0.95, rate(outbox_publish_lag_seconds_bucket[5m]))`
- `rate(consumer_retries_total[1m])`, `consumer_dead_letters_total`
- `histogram_quantile(0.95, rate(consumer_handle_seconds_bucket[1m]))`

Results on a laptop are not representative of a cloud deployment; record the machine and
the numbers here when you run it.
