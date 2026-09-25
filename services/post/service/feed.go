package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/feedplan"
	"socialai/shared/model"

	"github.com/olivere/elastic/v7"
)

// ErrBadCursor: the cursor query parameter could not be decoded (400, not 500).
var ErrBadCursor = errors.New("bad cursor")

// FeedPage is one page of the home feed. NextCursor is empty on the last page.
type FeedPage struct {
	Posts      []model.Post `json:"posts"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

func encodeCursor(it feedplan.Item) string {
	b, _ := json.Marshal(feedplan.CursorOf(it))
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (feedplan.Cursor, error) {
	var c feedplan.Cursor
	if s == "" {
		return c, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c)
	return c, err
}

// olderThan matches posts strictly after cursor c in (created_at desc, post_id desc) order.
// Posts written before created_at existed have no such field. ES sorts them last and the
// merge treats them as created_at 0, so they all come after any dated cursor; after an
// undated cursor (T == 0) only those with a smaller post_id remain.
func olderThan(c feedplan.Cursor) elastic.Query {
	undated := elastic.NewBoolQuery().MustNot(elastic.NewExistsQuery("created_at"))
	if c.T == 0 {
		undated = undated.Filter(elastic.NewRangeQuery("post_id").Lt(c.ID))
	}
	return elastic.NewBoolQuery().MinimumNumberShouldMatch(1).Should(
		elastic.NewRangeQuery("created_at").Lt(c.T),
		elastic.NewBoolQuery().Filter(
			elastic.NewTermQuery("created_at", c.T),
			elastic.NewRangeQuery("post_id").Lt(c.ID),
		),
		undated,
	)
}

// GetHomeFeed returns posts from the people userId follows, newest first.
//
// Read path of the hybrid feed (see shared/feedplan):
//  1. pushed part: the next limit+1 ids after the cursor from the user's sorted set, which
//     feed-worker fills (one bounded ZRANGE, not the whole feed);
//  2. pulled part: the next limit+1 posts after the cursor by followed accounts above the
//     celebrity threshold, which feed-worker does not push (one ES query, keyset paging on
//     (created_at, post_id));
//  3. both are merged by (created_at, post_id) and de-duplicated; the first limit items are
//     the page and item limit+1 decides whether there is a next page. Each source returned
//     its own first limit+1 items after the cursor, so the merge cannot miss or repeat one.
//  4. only the page's pushed ids are loaded from ES. Posts deleted since they were pushed
//     drop out here, so a page can be shorter than limit; the cursor is still exact.
func (s *PostService) GetHomeFeed(ctx context.Context, userId string, limit int, cursor string) (FeedPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cur, err := decodeCursor(cursor)
	if err != nil {
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
		authors := make([]interface{}, len(celebs))
		for i, c := range celebs {
			authors[i] = c
		}
		q := elastic.NewBoolQuery().
			Filter(elastic.NewTermsQuery("user_id", authors...)).
			MustNot(elastic.NewTermQuery("deleted", true))
		if cur.IsSet() {
			q = q.Filter(olderThan(cur))
		}
		res, err := s.es.SearchSorted(q, constants.POST_INDEX, "created_at", false, limit+1)
		if err != nil {
			return FeedPage{}, err
		}
		for _, p := range getPostFromSearchResult(res) {
			byID[p.PostId] = p
			pulled = append(pulled, feedplan.Item{PostID: p.PostId, AuthorID: p.UserId, CreatedAt: p.CreatedAt})
		}
	}

	// 3. Merge and cut.
	merged := feedplan.Before(feedplan.Merge(0, pushed, pulled), cur)
	page := FeedPage{Posts: []model.Post{}}
	if len(merged) > limit {
		page.NextCursor = encodeCursor(merged[limit-1])
		merged = merged[:limit]
	}

	// 4. Load the pushed posts on this page (one terms query, at most limit ids).
	var need []interface{}
	for _, it := range merged {
		if _, ok := byID[it.PostID]; !ok {
			need = append(need, it.PostID)
		}
	}
	if len(need) > 0 {
		q := elastic.NewBoolQuery().
			Filter(elastic.NewTermsQuery("post_id", need...)).
			MustNot(elastic.NewTermQuery("deleted", true))
		res, err := s.es.SearchSorted(q, constants.POST_INDEX, "created_at", false, len(need))
		if err != nil {
			return FeedPage{}, err
		}
		for _, p := range getPostFromSearchResult(res) {
			byID[p.PostId] = p
		}
	}
	for _, it := range merged {
		if p, ok := byID[it.PostID]; ok {
			page.Posts = append(page.Posts, p.Public())
		}
	}
	return page, nil
}

// followedCelebrities intersects the accounts userId follows with the celebrity set that
// feed-worker maintains.
func (s *PostService) followedCelebrities(ctx context.Context, userId string) ([]string, error) {
	celebSet, err := s.redis.SMembers(ctx, backend.CelebritySetKey)
	if err != nil && !backend.IsNil(err) {
		return nil, err
	}
	if len(celebSet) == 0 {
		return nil, nil
	}
	isCeleb := make(map[string]bool, len(celebSet))
	for _, c := range celebSet {
		isCeleb[c] = true
	}
	res, err := s.es.ReadFromESWithSize(elastic.NewTermQuery("follower_id", userId), constants.FOLLOW_INDEX, 5000)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, hit := range res.Hits.Hits {
		var f model.Follow
		if json.Unmarshal(hit.Source, &f) == nil && f.FollowerId == userId && isCeleb[f.FolloweeId] {
			out = append(out, f.FolloweeId)
		}
	}
	return out, nil
}
