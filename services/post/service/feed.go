package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"socialai/shared/backend"
	"socialai/shared/feedplan"
	"socialai/shared/model"
	"socialai/shared/pagecursor"
)

// ErrBadCursor: the cursor query parameter could not be decoded (400, not 500).
var ErrBadCursor = errors.New("bad cursor")

// FeedPage is one page of the home feed. NextCursor is empty on the last page.
type FeedPage struct {
	Posts      []model.Post `json:"posts"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// GetHomeFeed returns posts from the people userId follows, newest first.
//
// Read path of the hybrid feed (see shared/feedplan):
//  1. pushed part: the next limit+1 ids after the cursor from the user's Redis sorted set,
//     which feed-worker fills (one bounded ZRANGE, not the whole feed);
//  2. pulled part: the next limit+1 posts after the cursor by followed accounts above the
//     celebrity threshold, which feed-worker does not push (one keyset query on
//     posts_by_author);
//  3. both are merged by (created_at, post_id) and de-duplicated; the first limit items are
//     the page and item limit+1 decides whether there is a next page. Each source returned
//     its own first limit+1 items after the cursor, so the merge cannot miss or repeat one.
//  4. the page's pushed ids are loaded from PostgreSQL. Posts deleted since they were pushed
//     drop out here, so a page can be shorter than limit; the cursor is still exact.
func (s *PostService) GetHomeFeed(ctx context.Context, userId string, limit int, cursor string) (FeedPage, error) {
	limit = pagecursor.Limit(limit, 20, 100)
	var cur feedplan.Cursor
	if err := pagecursor.Decode(cursor, &cur); err != nil {
		return FeedPage{}, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}

	// 1. Pushed.
	pushed, err := s.redis.FeedItems(ctx, backend.HomeFeedKey(userId), cur, limit+1)
	if err != nil && !backend.IsNil(err) {
		return FeedPage{}, err
	}

	// 2. Pulled.
	byID := map[string]model.Post{}
	var pulled []feedplan.Item
	celebs, err := s.followedCelebrities(ctx, userId)
	if err != nil {
		return FeedPage{}, err
	}
	if len(celebs) > 0 {
		var after *time.Time // NULL = first page
		if cur.IsSet() {
			t := time.Unix(cur.T, 0)
			after = &t
		}
		posts, err := queryPosts(ctx, s.db, `
			SELECT `+postColumns+` FROM posts
			WHERE user_id = ANY($1) AND deleted_at IS NULL
			  AND ($2::timestamptz IS NULL OR (created_at, post_id) < ($2, $3))
			ORDER BY created_at DESC, post_id DESC
			LIMIT $4`, celebs, after, cur.ID, limit+1)
		if err != nil {
			return FeedPage{}, err
		}
		for _, p := range posts {
			byID[p.PostId] = p
			pulled = append(pulled, feedplan.Item{PostID: p.PostId, AuthorID: p.UserId, CreatedAt: p.CreatedAt})
		}
	}

	// 3. Merge and cut.
	merged := feedplan.Before(feedplan.Merge(0, pushed, pulled), cur)
	page := FeedPage{Posts: []model.Post{}}
	if len(merged) > limit {
		page.NextCursor = pagecursor.Encode(feedplan.CursorOf(merged[limit-1]))
		merged = merged[:limit]
	}

	// 4. Load the pushed posts on this page (one query, at most limit ids).
	var need []string
	for _, it := range merged {
		if _, ok := byID[it.PostID]; !ok {
			need = append(need, it.PostID)
		}
	}
	loaded, err := livePostsByID(ctx, s.db, need)
	if err != nil {
		return FeedPage{}, err
	}
	for _, p := range loaded {
		byID[p.PostId] = p
	}
	for _, it := range merged {
		if p, ok := byID[it.PostID]; ok {
			page.Posts = append(page.Posts, p.Public())
		}
	}
	return page, nil
}

// followedCelebrities intersects the celebrity set that feed-worker maintains with the
// accounts userId follows (a primary-key probe per celebrity, not a scan of the follow list).
func (s *PostService) followedCelebrities(ctx context.Context, userId string) ([]string, error) {
	celebs, err := s.redis.SMembers(ctx, backend.CelebritySetKey)
	if err != nil && !backend.IsNil(err) {
		return nil, err
	}
	return s.graph.FollowedAmong(ctx, userId, celebs)
}
