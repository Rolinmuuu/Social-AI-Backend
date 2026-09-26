// Package socialgraph holds the read-only queries over the social graph (the users table
// owned by auth, the follows table owned by social) that other services need: feed fan-out
// reads an author's followers, the home feed reads whom the reader follows, messaging checks
// that the recipient exists.
//
// Reads only. Writes stay with the owning service (services/auth, services/social), which is
// what lets these tables move to their own database later: the readers would then switch to
// an API call or a replicated copy without the writers changing.
package socialgraph

import (
	"context"
	"time"

	"socialai/shared/db"

	"github.com/jackc/pgx/v5"
)

// Graph runs the queries on DB (a pool or a transaction).
type Graph struct {
	DB db.Querier
}

// Cursor is the keyset position in a follow list ordered by (created_at, user id) descending.
type Cursor struct {
	T  int64  `json:"t"` // created_at in unix microseconds
	ID string `json:"id"`
}

// IsSet reports whether c points somewhere (the zero cursor means "first page").
func (c Cursor) IsSet() bool { return c.ID != "" }

// Page is one page of user ids and the cursor for the next page ("" on the last page).
type Page struct {
	IDs  []string
	Next *Cursor
}

// UserExists reports whether a user with this id has signed up.
func (g Graph) UserExists(ctx context.Context, userID string) (bool, error) {
	var ok bool
	err := g.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE user_id = $1)`, userID).Scan(&ok)
	return ok, err
}

// FollowerCountUpTo counts userID's followers but stops at max: enough to decide push vs
// pull for fan-out without counting a celebrity's millions of rows.
func (g Graph) FollowerCountUpTo(ctx context.Context, userID string, max int) (int, error) {
	var n int
	err := g.DB.QueryRow(ctx, `
		SELECT count(*) FROM (SELECT 1 FROM follows WHERE followee_id = $1 LIMIT $2) f`,
		userID, max).Scan(&n)
	return n, err
}

// Followers returns one page of the users following userID, newest first.
func (g Graph) Followers(ctx context.Context, userID string, after Cursor, limit int) (Page, error) {
	return g.page(ctx, `
		SELECT follower_id, created_at FROM follows
		WHERE followee_id = $1 AND ($2::timestamptz IS NULL OR (created_at, follower_id) < ($2, $3))
		ORDER BY created_at DESC, follower_id DESC
		LIMIT $4`, userID, after, limit)
}

// Following returns one page of the users userID follows, newest first.
func (g Graph) Following(ctx context.Context, userID string, after Cursor, limit int) (Page, error) {
	return g.page(ctx, `
		SELECT followee_id, created_at FROM follows
		WHERE follower_id = $1 AND ($2::timestamptz IS NULL OR (created_at, followee_id) < ($2, $3))
		ORDER BY created_at DESC, followee_id DESC
		LIMIT $4`, userID, after, limit)
}

func (g Graph) page(ctx context.Context, sql, userID string, after Cursor, limit int) (Page, error) {
	var t *time.Time // NULL = first page
	if after.IsSet() {
		at := time.UnixMicro(after.T)
		t = &at
	}
	// One extra row tells us whether there is a next page.
	rows, err := g.DB.Query(ctx, sql, userID, t, after.ID, limit+1)
	if err != nil {
		return Page{}, err
	}
	type row struct {
		id string
		at time.Time
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.at)
		return x, err
	})
	if err != nil {
		return Page{}, err
	}
	p := Page{IDs: []string{}}
	for i, x := range got {
		if i == limit {
			last := got[limit-1]
			p.Next = &Cursor{T: last.at.UnixMicro(), ID: last.id}
			break
		}
		p.IDs = append(p.IDs, x.id)
	}
	return p, nil
}

// AllFollowers reads up to max followers of userID in pages (fan-out is bounded by the
// celebrity threshold, so max is small).
func (g Graph) AllFollowers(ctx context.Context, userID string, max int) ([]string, error) {
	var out []string
	var cur Cursor
	for len(out) < max {
		n := 1000
		if rest := max - len(out); rest < n {
			n = rest
		}
		p, err := g.Followers(ctx, userID, cur, n)
		if err != nil {
			return nil, err
		}
		out = append(out, p.IDs...)
		if p.Next == nil {
			break
		}
		cur = *p.Next
	}
	return out, nil
}

// FollowedAmong returns the subset of candidates that userID follows (one index probe per
// candidate on the primary key).
func (g Graph) FollowedAmong(ctx context.Context, userID string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	rows, err := g.DB.Query(ctx, `
		SELECT followee_id FROM follows WHERE follower_id = $1 AND followee_id = ANY($2)`,
		userID, candidates)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
