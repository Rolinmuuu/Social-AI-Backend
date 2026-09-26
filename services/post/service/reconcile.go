package service

import (
	"context"
	"fmt"
	"time"

	"socialai/shared/backend"

	"github.com/jackc/pgx/v5"
)

// Reconciler repairs like_count / share_count from the post_likes and post_shares rows,
// which are the source of truth. Drift comes from the write-behind counter: a crash between
// taking a delta out of Redis and applying it loses that delta (see shared/counter).
//
// A count is only rewritten when it is safe to: the post has had no like or share for Quiet,
// and Redis holds no unflushed delta for it. Otherwise a like committed a moment ago, whose
// delta is still on its way, would be counted once by the recount and again by the flush.
// Hot posts are therefore repaired once they cool down. The UPDATE is conditional on the
// counts read, so a flush that lands in between wins and the post is re-checked next pass.
type Reconciler struct {
	svc   *PostService
	Batch int           // posts checked per pass (default 500)
	Quiet time.Duration // default 5 minutes
	after string        // keyset position; the table is walked in post_id order, then wraps
}

// NewReconciler returns a reconciler for the service's posts.
func (s *PostService) NewReconciler() *Reconciler { return &Reconciler{svc: s} }

// ReconcileResult summarises one pass.
type ReconcileResult struct {
	Checked int
	Fixed   int
	Skipped int // drifted but busy or with an unflushed delta
}

// RunOnce checks the next batch of posts and repairs the quiet ones that drifted.
func (r *Reconciler) RunOnce(ctx context.Context) (ReconcileResult, error) {
	batch, quiet := r.Batch, r.Quiet
	if batch <= 0 {
		batch = 500
	}
	if quiet <= 0 {
		quiet = 5 * time.Minute
	}
	var res ReconcileResult
	rows, err := r.svc.db.Query(ctx, `
		SELECT p.post_id, p.like_count, p.share_count,
		       (SELECT count(*) FROM post_likes l WHERE l.post_id = p.post_id),
		       (SELECT count(*) FROM post_shares s WHERE s.post_id = p.post_id),
		       EXISTS (SELECT 1 FROM post_likes l WHERE l.post_id = p.post_id AND l.created_at > now() - make_interval(secs => $3))
		    OR EXISTS (SELECT 1 FROM post_shares s WHERE s.post_id = p.post_id AND s.created_at > now() - make_interval(secs => $3))
		FROM posts p
		WHERE p.post_id > $1
		ORDER BY p.post_id
		LIMIT $2`, r.after, batch, quiet.Seconds())
	if err != nil {
		return res, err
	}
	type check struct {
		id                         string
		likes, shares, nLike, nShr int64
		busy                       bool
	}
	checks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (check, error) {
		var c check
		err := row.Scan(&c.id, &c.likes, &c.shares, &c.nLike, &c.nShr, &c.busy)
		return c, err
	})
	if err != nil {
		return res, err
	}
	if len(checks) < batch {
		r.after = "" // reached the end: next pass starts over
	} else {
		r.after = checks[len(checks)-1].id
	}

	for _, c := range checks {
		res.Checked++
		if c.likes == c.nLike && c.shares == c.nShr {
			continue
		}
		pending, err := r.hasPendingDelta(ctx, c.id)
		if err != nil {
			return res, err
		}
		if c.busy || pending {
			res.Skipped++
			continue
		}
		tag, err := r.svc.db.Exec(ctx, `
			UPDATE posts SET like_count = $2, share_count = $3
			WHERE post_id = $1 AND like_count = $4 AND share_count = $5`,
			c.id, c.nLike, c.nShr, c.likes, c.shares)
		if err != nil {
			return res, fmt.Errorf("reconcile %s: %w", c.id, err)
		}
		if tag.RowsAffected() == 1 {
			res.Fixed++
		} else {
			res.Skipped++
		}
	}
	return res, nil
}

func (r *Reconciler) hasPendingDelta(ctx context.Context, postID string) (bool, error) {
	for field := range counterColumns {
		v, err := r.svc.redis.Get(ctx, r.svc.Counters.PendingKey(postID, field))
		if backend.IsNil(err) {
			continue
		}
		if err != nil {
			// Redis unreachable: we cannot rule out a pending delta, so leave the post alone.
			return true, nil
		}
		if v != "" && v != "0" {
			return true, nil
		}
	}
	return false, nil
}
