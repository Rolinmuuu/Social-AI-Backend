-- PostgreSQL is the system of record. Elasticsearch keeps only a search projection of posts,
-- rebuilt from these tables (services/indexer). See docs/adr/0001-postgres-system-of-record.md.
--
-- Ownership: every table has exactly one service that writes it (named in the section
-- header). Other services may read it; they never write it. Foreign keys only link tables
-- with the same owner, so a service's tables can later move to their own database.

-- ─── auth ──────────────────────────────────────────────────────────────────────────────
CREATE TABLE users (
    user_id       text PRIMARY KEY,
    username      text        NOT NULL DEFAULT '',
    password_hash text        NOT NULL,
    age           bigint      NOT NULL DEFAULT 0,
    gender        text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- ─── social ────────────────────────────────────────────────────────────────────────────
-- The primary key is the relationship itself, so "follow twice" cannot happen, however
-- many requests race (the old ES version checked, then wrote a document with a random id).
CREATE TABLE follows (
    follower_id text        NOT NULL,
    followee_id text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (follower_id, followee_id),
    CHECK (follower_id <> followee_id)
);
-- Followers of a user, newest first: the followers list and feed fan-out page through it.
CREATE INDEX follows_followers ON follows (followee_id, created_at DESC, follower_id DESC);
-- Accounts a user follows, newest first.
CREATE INDEX follows_following ON follows (follower_id, created_at DESC, followee_id DESC);

-- ─── post ──────────────────────────────────────────────────────────────────────────────
CREATE TABLE posts (
    post_id          text PRIMARY KEY,
    user_id          text        NOT NULL,
    message          text        NOT NULL DEFAULT '',
    url              text        NOT NULL DEFAULT '',
    type             text        NOT NULL DEFAULT '',
    -- Whole seconds: home-feed cursors and Redis feed scores are unix seconds.
    created_at       timestamptz NOT NULL DEFAULT date_trunc('second', now()),
    deleted_at       timestamptz,
    cleanup_status   text        NOT NULL DEFAULT ''
                     CHECK (cleanup_status IN ('', 'pending', 'completed', 'failed')),
    cleanup_attempts int         NOT NULL DEFAULT 0,
    cleanup_error    text        NOT NULL DEFAULT '',
    like_count       bigint      NOT NULL DEFAULT 0,
    share_count      bigint      NOT NULL DEFAULT 0,
    -- OpenAI embedding of message. Stored here so rebuilding the search index never pays for
    -- embeddings twice. Filled asynchronously by the indexer.
    embedding        real[],
    -- Bumped on every change the search index must see; the indexer writes with
    -- version_type=external, so a stale or redelivered event can never overwrite newer state.
    version          bigint      NOT NULL DEFAULT 1
);
CREATE INDEX posts_by_author ON posts (user_id, created_at DESC, post_id DESC) WHERE deleted_at IS NULL;
CREATE INDEX posts_cleanup_due ON posts (deleted_at) WHERE cleanup_status = 'pending';

CREATE TABLE post_likes (
    post_id    text        NOT NULL REFERENCES posts ON DELETE CASCADE,
    user_id    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (post_id, user_id)
);
-- "Any like in the last N minutes?" (the like-count reconciler only fixes quiet posts).
CREATE INDEX post_likes_recent ON post_likes (post_id, created_at);

CREATE TABLE post_shares (
    share_id   text PRIMARY KEY,
    post_id    text        NOT NULL REFERENCES posts ON DELETE CASCADE,
    user_id    text        NOT NULL,
    platform   text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX post_shares_recent ON post_shares (post_id, created_at);

CREATE TABLE comments (
    comment_id        text PRIMARY KEY,
    post_id           text        NOT NULL REFERENCES posts ON DELETE CASCADE,
    parent_comment_id text,
    root_comment_id   text        NOT NULL,
    depth             int         NOT NULL DEFAULT 0 CHECK (depth >= 0),
    user_id           text        NOT NULL,
    content           text        NOT NULL CHECK (length(content) BETWEEN 1 AND 2000),
    created_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,
    UNIQUE (post_id, comment_id),
    -- A reply's parent must be a comment on the same post (enforced by the database, not by
    -- a read-then-check in the service).
    FOREIGN KEY (post_id, parent_comment_id) REFERENCES comments (post_id, comment_id)
);
CREATE INDEX comments_by_post ON comments (post_id, created_at, comment_id);

-- Transactional outbox: rows are inserted in the same transaction as the change they
-- describe, and the relay (shared/outbox) publishes them to Kafka. Column names follow the
-- Debezium outbox event router (aggregatetype / aggregateid / type / payload) closely enough
-- that the polling relay could be swapped for log-based CDC without touching writers.
CREATE TABLE outbox (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_type  text        NOT NULL,
    aggregate_id    text        NOT NULL,
    topic           text        NOT NULL,
    partition_key   text        NOT NULL,
    payload         jsonb       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    status          text        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'published', 'dead')),
    attempts        int         NOT NULL DEFAULT 0,
    -- Earliest time the relay may (re)try. Also the lease: a relay that claims a row pushes
    -- it forward, so a crashed relay's rows become due again once the lease runs out.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text        NOT NULL DEFAULT '',
    published_at    timestamptz
);
CREATE INDEX outbox_due ON outbox (next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX outbox_prunable ON outbox (published_at) WHERE status = 'published';

-- ─── notification ──────────────────────────────────────────────────────────────────────
CREATE TABLE notifications (
    notification_id text PRIMARY KEY, -- deterministic, so a redelivered event is a no-op
    user_id         text        NOT NULL,
    type            text        NOT NULL,
    actor_id        text        NOT NULL,
    post_id         text        NOT NULL DEFAULT '',
    read            boolean     NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_inbox ON notifications (user_id, created_at DESC, notification_id DESC);

-- ─── message ───────────────────────────────────────────────────────────────────────────
-- A direct conversation has a deterministic id ("dm:<a>:<b>", a < b), so both participants
-- land on the same row without a lookup table.
CREATE TABLE conversations (
    conversation_id text PRIMARY KEY,
    kind            text        NOT NULL DEFAULT 'direct' CHECK (kind IN ('direct')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Per-conversation sequence. Sending a message increments it under the row lock, so
    -- sequence order is commit order (see messages below).
    last_seq        bigint      NOT NULL DEFAULT 0,
    last_message_at timestamptz
);

CREATE TABLE conversation_members (
    conversation_id text   NOT NULL REFERENCES conversations ON DELETE CASCADE,
    user_id         text   NOT NULL,
    last_read_seq   bigint NOT NULL DEFAULT 0,
    -- Copy of conversations.last_message_at, so a user's inbox is one index range scan.
    last_message_at timestamptz,
    PRIMARY KEY (conversation_id, user_id)
);
CREATE INDEX conversation_members_inbox
    ON conversation_members (user_id, last_message_at DESC NULLS LAST, conversation_id DESC);

-- Messages are clustered by conversation and ordered by seq. created_at is kept for display
-- but is not the paging key: two senders' transactions can commit in the opposite order of
-- their timestamps, and a client that polls "messages after created_at X" would then skip
-- the one that committed late. seq is assigned under the conversation row lock, so it is
-- gap-free and increases in commit order.
CREATE TABLE messages (
    conversation_id text        NOT NULL REFERENCES conversations ON DELETE CASCADE,
    seq             bigint      NOT NULL,
    sender_id       text        NOT NULL,
    content         text        NOT NULL CHECK (length(content) BETWEEN 1 AND 4000),
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Idempotency-Key of the send request: a retried send returns the first message.
    client_msg_id   text,
    PRIMARY KEY (conversation_id, seq)
);
CREATE UNIQUE INDEX messages_client_msg_id
    ON messages (conversation_id, sender_id, client_msg_id) WHERE client_msg_id IS NOT NULL;
