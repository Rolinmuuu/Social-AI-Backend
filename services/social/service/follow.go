package service

import (
	"context"
	"fmt"

	"socialai/shared/pagecursor"
	"socialai/shared/socialgraph"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SocialService owns the follows table.
type SocialService struct {
	db    *pgxpool.Pool
	graph socialgraph.Graph
}

func NewSocialService(pool *pgxpool.Pool) *SocialService {
	return &SocialService{db: pool, graph: socialgraph.Graph{DB: pool}}
}

// FollowPage is one page of a follower or following list.
type FollowPage struct {
	IDs        []string
	NextCursor string
}

// AddFollow creates a follow relationship. Returns ErrAlreadyFollowing if it exists and
// ErrUserNotFound if followee has not signed up.
//
// The primary key (follower_id, followee_id) makes "follow" a single atomic insert. The
// Elasticsearch version searched first and then indexed a document under a random id, so a
// double click could store the relationship twice (and unfollow then removed only one).
func (s *SocialService) AddFollow(ctx context.Context, followerId, followeeId string) (string, error) {
	if followerId == followeeId {
		return "", ErrCannotFollowSelf
	}
	exists, err := s.graph.UserExists(ctx, followeeId)
	if err != nil {
		return "", fmt.Errorf("failed to look up user: %w", err)
	}
	if !exists {
		return "", ErrUserNotFound
	}
	tag, err := s.db.Exec(ctx, `
		INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2)
		ON CONFLICT (follower_id, followee_id) DO NOTHING`, followerId, followeeId)
	if err != nil {
		return "", fmt.Errorf("failed to save follow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrAlreadyFollowing
	}
	return FollowID(followerId, followeeId), nil
}

// FollowID is the public id of a relationship (its primary key).
func FollowID(followerId, followeeId string) string { return followerId + ":" + followeeId }

// RemoveFollow deletes a follow relationship. Returns ErrNotFollowing if it doesn't exist.
func (s *SocialService) RemoveFollow(ctx context.Context, followerId, followeeId string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2`, followerId, followeeId)
	if err != nil {
		return fmt.Errorf("failed to delete follow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFollowing
	}
	return nil
}

// Followers returns one page of userId's followers, newest first.
//
// The Elasticsearch version sent no size, so it silently returned the first 10 followers
// (the search default) to everyone, whatever the real count.
func (s *SocialService) Followers(ctx context.Context, userId string, limit int, cursor string) (FollowPage, error) {
	return s.list(ctx, s.graph.Followers, userId, limit, cursor)
}

// Following returns one page of the accounts userId follows, newest first.
func (s *SocialService) Following(ctx context.Context, userId string, limit int, cursor string) (FollowPage, error) {
	return s.list(ctx, s.graph.Following, userId, limit, cursor)
}

type lister func(ctx context.Context, userID string, after socialgraph.Cursor, limit int) (socialgraph.Page, error)

func (s *SocialService) list(ctx context.Context, fn lister, userId string, limit int, cursor string) (FollowPage, error) {
	var after socialgraph.Cursor
	if err := pagecursor.Decode(cursor, &after); err != nil {
		return FollowPage{}, err
	}
	p, err := fn(ctx, userId, after, pagecursor.Limit(limit, 50, 200))
	if err != nil {
		return FollowPage{}, err
	}
	out := FollowPage{IDs: p.IDs}
	if p.Next != nil {
		out.NextCursor = pagecursor.Encode(p.Next)
	}
	return out, nil
}
