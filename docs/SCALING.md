# Bottlenecks, scaling and evidence

This page lists where the system saturates first, what was changed, and how each claim is
verified. "Verified" means an automated test in this repository. Everything that touches SQL
runs against **a real PostgreSQL** (a fresh schema per test, `shared/db/dbtest`), so
constraints, `ON CONFLICT`, row locks and `SKIP LOCKED` are exercised for real, including
under concurrency. Redis, Elasticsearch and Kafka are in-memory doubles; the Elasticsearch
double evaluates the `bool`/`term`/`terms`/`range`/`exists` filters the services build and
implements external versioning, and `go test -tags=integration ./shared/backend/` checks the
same behaviour against a real cluster. The doubles do not reproduce refresh delay or network
latency. Numbers that need a real deployment are marked **to measure**; no latency or
throughput figure has been measured yet.

## Where it saturates, in order

| # | Hot spot | Symptom | Change | Evidence |
|---|---|---|---|---|
| 1 | Fan-out of one post to many followers | feed-worker lag grows with follower count; 3 Redis round trips per follower | pipelined batches of 500, 4 in flight; push/pull switch above 5,000 followers; follower count capped at 5,001 rows | `TestFanOutBatchesAndBoundsConcurrency` (4,321 followers → 9 round trips instead of 12,963), `TestHandlePostCreated_BatchesFollowerWrites` (2,500 follower rows in PostgreSQL → 25 round trips), `TestHandlePostCreated_CelebrityIsPulledNotPushed` |
| 2 | Likes on one popular post | every like waiting on the post row's lock (ES: version conflicts, 500s) | like = one primary-key insert; count via Redis `INCRBY` + periodic flush: one `UPDATE` per post per flush per instance | `TestThousandConcurrentLikesBecomeOneWrite` (1,000 concurrent likes → 1 write of +1000), `TestLikePost_ConcurrentDoubleTapCountsOnce` |
| 3 | Cache expiry of a hot key | burst of identical queries | single-flight + TTL jitter | `TestSearchPostByUserId_ConcurrentMissesShareOneQuery` (50 concurrent misses → ≤ 2 queries), `TestConcurrentMissesQueryOnce` |
| 4 | Synchronous work on the upload path | an OpenAI embedding call (hundreds of ms) and a Kafka publish on every upload | embedding moved to the search indexer; inline publish capped at 1 s, failures left to the relay | `TestSavePost_Success` (no embedding call), `TestSavePost_KafkaDown_PostSavedAndEventPublishedLater`; latency **to measure** (`upload` scenario) |
| 5 | Outbox relay with several instances | relays waiting on each other's rows, or publishing the same rows | `FOR UPDATE SKIP LOCKED` claims with a lease; no transaction open during publish | `TestConcurrentRelaysPublishEachEventOnce` (8 relays, 400 events, each once) |
| 6 | Consumer throughput | one message at a time per instance | 8 keyed workers per instance, bounded queues, horizontal scale via consumer group | `TestPerKeyOrderIsKeptWhileKeysRunInParallel` |
| 7 | Home feed read | loaded the whole feed (up to 500 posts) on every page | next `limit+1` ids from Redis, pulled posts by keyset on `posts_by_author`, only the page's posts loaded | `TestGetHomeFeed_PagingNeverRepeatsOrSkips`; latency **to measure** (`browse` scenario) |
| 8 | Search payload | every hit carried a 1,536-float vector | source filter excludes it; hits are ids, rows come from PostgreSQL | code review; payload size **to measure** |
| 9 | Lists (followers, messages, a user's posts) | silently truncated at 10 (Elasticsearch default size) | keyset pagination on covering indexes | `TestFollowers_PagesThroughEveryone`, `TestPagingBackAndCatchingUp` |

## Correctness under failure and concurrency

| Guarantee | Test |
|---|---|
| A post and its event commit together; if the event cannot be recorded the post is rolled back | `TestEnqueueCommitsOrRollsBackWithTheBusinessWrite`, `TestSavePost_OutboxFailureRollsBackThePost` |
| Kafka outage does not fail uploads or likes; their events are published once the broker is back | `TestSavePost_KafkaDown_PostSavedAndEventPublishedLater`, `TestLikePost_KafkaDownNotificationIsDelayedNotLost`, `TestPGRelaySurvivesBrokerOutage`, `TestBrokerOutageDoesNotLoseEvents` |
| A relay that dies after claiming rows does not strand them | `TestClaimedRowsComeBackWhenTheLeaseExpires` |
| A 16-hour outage only delays an event; it is never given up on (default settings) | `TestLongOutageNeverParksByDefault` |
| Shutdown mid-pass does not use up a record's retry attempts | `TestCancelledContextDoesNotSpendAnAttempt` |
| With parking enabled, a poison event is parked, never dropped, and does not block others | `TestPoisonRecordIsParkedNotDropped`, `TestPoisonMessageGoesToDLQAndDoesNotBlockThePartition` |
| No offset is committed past a message that was neither handled nor dead-lettered | `TestNothingIsCommittedPastAMessageThatCouldNotBeParked`, `TestTrackerCommitsOnlyContiguousOffsets` |
| Ten concurrent sign-ups with one id: one account, and it keeps its password | `TestAddUser_ConcurrentSignupsOneWinner` |
| Twenty concurrent follows / likes by one user: one row, one event | `TestAddFollow_ConcurrentDoubleClickStoresOneRow`, `TestLikePost_ConcurrentDoubleTapCountsOnce` |
| Redelivered events change nothing (feed, notification, search) | `TestHandlePostCreated_RedeliveryIsIdempotent`, `TestHandlePostLiked_RedeliveryIsANoOp`, `TestRedeliveryIsANoOp` |
| A late `post.created` cannot bring a deleted post back into search; stale index writes are refused | `TestLateCreatedEventCannotResurrectADeletedPost`, `TestStaleWriteIsRefusedByVersion` |
| A lagging index never shows a deleted post or a stale count | `TestSearchPostByKeywords_HydratesFromTheDatabase`, `TestSearchPostByKeywords_TombstonesAreNotMatched` |
| An OpenAI outage does not dead-letter indexing; vectors are filled in later | `TestOpenAIOutageIndexesWithoutVectorAndRepairsLater` |
| 100 concurrent sends in both directions into a new conversation: seq 1..100, gap-free, commit-ordered | `TestConcurrentSendersGetGapFreeCommitOrderedSeqs` |
| Ten concurrent retries of one message send store it once | `TestConcurrentRetriesStoreOneMessage` |
| 50 concurrent retries with one Idempotency-Key run the upload handler once; later retries replay | `TestConcurrentRetriesRunTheHandlerOnce`, `TestUploadRetryWithSameKeyCreatesOnePost` |
| Client disconnects after the post is saved, then retries: one post, the retry replays | `TestUploadClientDisconnectThenRetryReplays` |
| Deleting a post does not overwrite its counters; deleting twice records one event | `TestDeletePost_DoesNotOverwriteCounters`, `TestDeletePost_Success` |
| Several instances cleaning up media process each post once; failures stop after 5 attempts | `TestConcurrentCleanupClaimsEachPostOnce`, `TestCleanupGivesUpAfterFiveFailures` |
| SIGTERM during a counter flush loses no delta; a half-recorded like is never counted twice | `TestShutdownDuringFlushLosesNothing`, `TestIncrThatCannotScheduleIsUndone` |
| Counter drift is repaired from the rows, but never while a delta is in flight | `TestReconcilerRepairsDriftedQuietPosts` |
| Feed paging over pushed + pulled posts with tied timestamps, epoch-dated and deleted posts: every post exactly once, in order | `TestGetHomeFeed_PagingNeverRepeatsOrSkips`, `TestGetHomeFeed_MergesPushedAndCelebrityPosts`, `TestBeforeCursorPagesWithoutGapsOrRepeats` |
| Concurrent migrators apply each file once; a failed file leaves nothing; an edited file is refused | `TestConcurrentMigratorsApplyEachFileOnce`, `TestFailedMigrationLeavesNoTrace`, `TestEditedMigrationIsRefused` |
| The legacy data migration is idempotent and carries over events stuck in the old outbox | `TestBackfillCopiesAndCleansLegacyData`, `TestBackfillMessagesContinueAfterNewTraffic` |

CI runs `go vet` and all tests with the race detector (`go test -race`) against a PostgreSQL
service container.

## Back-of-envelope capacity (assumptions stated)

Assumptions: 1M users, 10% daily active, each reads the feed 10×/day and posts 0.2×/day;
average 200 followers; peak = 5× average.

| Quantity | Estimate |
|---|---|
| Posts/day | 100k DAU × 0.2 = 20k → ~0.23/s average, ~1.2/s peak |
| Feed writes (push) | 20k × 200 = 4M `ZADD`/day. Pipelines are per post, and an average post (200 followers) fits in one, so ≈ 20k round trips/day (~1.2/s at peak) — trivial for one Redis |
| Feed reads | 100k × 10 = 1M/day → ~12/s average, ~60/s peak; per page: one bounded `ZRANGE`, one primary-key probe per celebrity, one keyset query for pulled posts, one `ANY($ids)` lookup for at most `limit` posts |
| Feed memory | a feed exists for every follower who received a push, not only daily actives, and each push refreshes its 7-day TTL. Upper bound: 500 entries × ~100 B (36-char UUID member + score + skiplist overhead) ≈ 50 KB per feed. With ~500k users receiving a push in any week that is up to ~25 GB; in practice far less, because most feeds hold well under 500 entries. Worth measuring with `MEMORY USAGE` before sizing, and the reason `MaxFeedLen` and the TTL are knobs |
| Likes | the hot-post case matters more than the average: each like is one insert into `post_likes`; write-behind caps `posts` row updates at 1 per post per 2 s per post-service instance, regardless of like rate |
| PostgreSQL size | rows are small: 1M users × 200 follows ≈ 200M `follows` rows. At ~60 B per heap row (tuple header, two short ids, a timestamp) that is ~12 GB, plus three indexes of ~40–50 B per entry: roughly 40 GB in all. Posts, likes and messages grow with activity. One primary with a read replica covers this; `messages` is the first candidate for partitioning (by `conversation_id`) |

The first real limits at this scale are PostgreSQL write volume on the hottest rows (covered
by write-behind counters and short conversation locks), Elasticsearch for search and kNN, and
the celebrity threshold. Kafka and Redis are far from saturated. The estimates above are
arithmetic, not measurements.

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
