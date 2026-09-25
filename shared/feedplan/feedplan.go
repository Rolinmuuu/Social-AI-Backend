// Package feedplan holds the home-feed logic that does not depend on Redis or Elasticsearch:
// the push/pull decision, batched fan-out, and the read-time merge.
//
// Home feeds are built with a hybrid strategy:
//
//   - Push (fan-out on write) for normal accounts: when a post is created, its id is added to
//     each follower's materialised feed (a Redis sorted set scored by creation time). Reads are
//     a single ZREVRANGE.
//   - Pull (fan-out on read) for accounts above CelebrityThreshold followers: pushing one post
//     to millions of feeds would take minutes and write millions of keys. Their posts are not
//     pushed; each reader fetches recent posts of the celebrities they follow and merges them
//     into the materialised feed at read time.
//
// Writes are batched: one Redis pipeline per BatchSize followers instead of three round trips
// per follower, with a bounded number of batches in flight.
package feedplan

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Mode is how a new post reaches followers.
type Mode int

const (
	Push Mode = iota
	Pull
)

func (m Mode) String() string {
	if m == Pull {
		return "pull"
	}
	return "push"
}

// Policy configures fan-out.
type Policy struct {
	CelebrityThreshold int           // followers above this switch the author to Pull (default 5000)
	BatchSize          int           // followers per pipeline (default 500)
	Parallelism        int           // pipelines in flight (default 4)
	MaxFeedLen         int           // entries kept per materialised feed (default 500)
	FeedTTL            time.Duration // idle feeds expire (default 7 days)
}

func (p Policy) withDefaults() Policy {
	if p.CelebrityThreshold <= 0 {
		p.CelebrityThreshold = 5000
	}
	if p.BatchSize <= 0 {
		p.BatchSize = 500
	}
	if p.Parallelism <= 0 {
		p.Parallelism = 4
	}
	if p.MaxFeedLen <= 0 {
		p.MaxFeedLen = 500
	}
	if p.FeedTTL <= 0 {
		p.FeedTTL = 7 * 24 * time.Hour
	}
	return p
}

// Defaults returns the policy with every zero field filled in.
func (p Policy) Defaults() Policy { return p.withDefaults() }

// Decide picks Push or Pull from the author's follower count.
func (p Policy) Decide(followers int) Mode {
	if followers > p.withDefaults().CelebrityThreshold {
		return Pull
	}
	return Push
}

// Item is one feed entry: the post id and its creation time (the sort key).
type Item struct {
	PostID    string
	AuthorID  string
	CreatedAt int64 // unix seconds
}

// Writer adds one item to many followers' feeds in a single round trip (a Redis pipeline of
// ZADD + ZREMRANGEBYRANK + EXPIRE per follower). ZADD keyed by post id is idempotent, so a
// redelivered event does not create duplicates.
type Writer interface {
	AddToFeeds(ctx context.Context, followerIDs []string, item Item, maxLen int, ttl time.Duration) error
}

// Batches splits ids into chunks of at most size.
func Batches(ids []string, size int) [][]string {
	if size <= 0 {
		size = 1
	}
	out := make([][]string, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[start:end])
	}
	return out
}

// FanOut pushes item to every follower, BatchSize at a time with at most Parallelism batches
// in flight. It returns the number of round trips made and the first error; batches that
// already succeeded are not rolled back (ZADD is idempotent, so the redelivered event redoes
// only what is missing).
func FanOut(ctx context.Context, w Writer, followers []string, item Item, p Policy) (int, error) {
	p = p.withDefaults()
	batches := Batches(followers, p.BatchSize)
	sem := make(chan struct{}, p.Parallelism)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		trips    int
	)
	for _, b := range batches {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(batch []string) {
			defer wg.Done()
			defer func() { <-sem }()
			err := w.AddToFeeds(ctx, batch, item, p.MaxFeedLen, p.FeedTTL)
			mu.Lock()
			trips++
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(b)
	}
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	return trips, firstErr
}

// Merge combines feeds sorted newest-first into one newest-first list of at most limit items,
// dropping duplicate post ids (a post can be both materialised and pulled while an author
// crosses the threshold). Ties on time are broken by post id so pagination is stable.
func Merge(limit int, lists ...[]Item) []Item {
	seen := make(map[string]bool)
	var all []Item
	for _, l := range lists {
		for _, it := range l {
			if it.PostID == "" || seen[it.PostID] {
				continue
			}
			seen[it.PostID] = true
			all = append(all, it)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt != all[j].CreatedAt {
			return all[i].CreatedAt > all[j].CreatedAt
		}
		return all[i].PostID > all[j].PostID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// Cursor is the last item a client saw, (created_at, post_id). The zero Cursor means "start
// from the newest". Presence is decided by ID, not time: a post can have CreatedAt 0 (posts
// written before created_at existed), and treating T==0 as "no cursor" would restart the feed.
type Cursor struct {
	T  int64  `json:"t"`
	ID string `json:"id"`
}

// IsSet reports whether the cursor points at an item.
func (c Cursor) IsSet() bool { return c.ID != "" }

// CursorOf returns the cursor that resumes right after it.
func CursorOf(it Item) Cursor { return Cursor{T: it.CreatedAt, ID: it.PostID} }

// Before returns the items strictly older than the cursor in (created_at, post_id) order,
// which is how the feed endpoint pages: the client sends the last item it saw.
func Before(items []Item, c Cursor) []Item {
	if !c.IsSet() {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if it.CreatedAt < c.T || (it.CreatedAt == c.T && it.PostID < c.ID) {
			out = append(out, it)
		}
	}
	return out
}
