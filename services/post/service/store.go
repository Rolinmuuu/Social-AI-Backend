package service

import (
	"context"
	"errors"
	"time"

	"socialai/shared/db"
	"socialai/shared/model"

	"github.com/jackc/pgx/v5"
)

// postColumns is the column list every post query selects, in scanPost's order.
const postColumns = `post_id, user_id, message, url, type, created_at, deleted_at, like_count, share_count`

func scanPost(row pgx.CollectableRow) (model.Post, error) {
	var p model.Post
	var created time.Time
	var deleted *time.Time
	if err := row.Scan(&p.PostId, &p.UserId, &p.Message, &p.Url, &p.Type, &created, &deleted, &p.LikeCount, &p.SharedCount); err != nil {
		return p, err
	}
	p.CreatedAt = created.Unix()
	if deleted != nil {
		p.Deleted, p.DeletedAt = true, deleted.Unix()
	}
	return p, nil
}

func queryPosts(ctx context.Context, q db.Querier, sql string, args ...any) ([]model.Post, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPost)
}

// livePostsByID loads the non-deleted posts among ids and returns them in the order of ids
// (the ranking of a search result or a feed page). Missing and deleted ids are dropped: the
// search index and Redis feeds are allowed to lag, the database decides what exists.
func livePostsByID(ctx context.Context, q db.Querier, ids []string) ([]model.Post, error) {
	if len(ids) == 0 {
		return []model.Post{}, nil
	}
	found, err := queryPosts(ctx, q, `SELECT `+postColumns+` FROM posts WHERE post_id = ANY($1) AND deleted_at IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]model.Post, len(found))
	for _, p := range found {
		byID[p.PostId] = p
	}
	out := make([]model.Post, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
			delete(byID, id) // an id listed twice is returned once
		}
	}
	return out, nil
}

// postOwner returns the author of a live post, or ErrPostNotFound.
func postOwner(ctx context.Context, q db.Querier, postID string) (string, error) {
	var owner string
	err := q.QueryRow(ctx, `SELECT user_id FROM posts WHERE post_id = $1 AND deleted_at IS NULL`, postID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPostNotFound
	}
	return owner, err
}

// pgCounterSink applies write-behind counter deltas to the posts table.
type pgCounterSink struct{ db db.Querier }

// counterColumns maps counter field names to columns (and is the whitelist that keeps a
// field name from ever being interpolated into SQL unchecked).
var counterColumns = map[string]string{
	"like_count":   "like_count",
	"shared_count": "share_count",
}

func (s pgCounterSink) Add(ctx context.Context, postID, field string, delta int64) error {
	col, ok := counterColumns[field]
	if !ok {
		return nil // unknown field: nothing to apply
	}
	// A post deleted meanwhile still gets its count: the row is soft-deleted, not removed.
	_, err := s.db.Exec(ctx, `UPDATE posts SET `+col+` = `+col+` + $2 WHERE post_id = $1`, postID, delta)
	return err
}
