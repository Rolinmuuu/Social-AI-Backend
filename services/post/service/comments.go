package service

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"socialai/shared/db"
	"socialai/shared/model"
	"socialai/shared/pagecursor"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maxCommentLen = 2000

// AddComment adds a comment (or a reply, when parentCommentId is set) to a live post.
func (s *PostService) AddComment(ctx context.Context, postId, parentCommentId, userId, content string) (string, error) {
	if postId == "" || userId == "" || content == "" || utf8.RuneCountInString(content) > maxCommentLen {
		return "", ErrInvalidComment
	}
	if _, err := postOwner(ctx, s.db, postId); err != nil {
		return "", err
	}

	commentId := uuid.New().String()
	rootCommentId := commentId
	depth := 0
	var parent *string
	if parentCommentId != "" {
		var parentPost, root string
		var parentDepth int
		err := s.db.QueryRow(ctx, `
			SELECT post_id, root_comment_id, depth FROM comments
			WHERE comment_id = $1 AND deleted_at IS NULL`, parentCommentId).Scan(&parentPost, &root, &parentDepth)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && parentPost != postId) {
			return "", ErrCommentNotFound
		}
		if err != nil {
			return "", err
		}
		rootCommentId, depth, parent = root, parentDepth+1, &parentCommentId
	}

	_, err := s.db.Exec(ctx, `
		INSERT INTO comments (comment_id, post_id, parent_comment_id, root_comment_id, depth, user_id, content)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		commentId, postId, parent, rootCommentId, depth, userId, content)
	switch {
	case db.IsForeignKeyViolation(err):
		// The post was hard-deleted or the parent moved between the check and the insert:
		// the composite foreign key (post_id, parent_comment_id) refuses the orphan.
		return "", ErrCommentNotFound
	case err != nil:
		return "", fmt.Errorf("failed to save comment: %w", err)
	}
	return commentId, nil
}

// CommentPage is one page of a post's comments, oldest first.
type CommentPage struct {
	Comments   []model.Comment `json:"comments"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type commentCursor struct {
	T  int64  `json:"t"` // created_at, unix microseconds
	ID string `json:"id"`
}

// ListComments returns the comments on a live post in the order they were written.
func (s *PostService) ListComments(ctx context.Context, postId string, limit int, cursor string) (CommentPage, error) {
	var cur commentCursor
	if err := pagecursor.Decode(cursor, &cur); err != nil {
		return CommentPage{}, fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	limit = pagecursor.Limit(limit, 50, 200)
	if _, err := postOwner(ctx, s.db, postId); err != nil {
		return CommentPage{}, err
	}
	var after *time.Time
	if cur.ID != "" {
		t := time.UnixMicro(cur.T)
		after = &t
	}
	rows, err := s.db.Query(ctx, `
		SELECT comment_id, coalesce(parent_comment_id, ''), root_comment_id, user_id, post_id, depth, content, created_at
		FROM comments
		WHERE post_id = $1 AND deleted_at IS NULL
		  AND ($2::timestamptz IS NULL OR (created_at, comment_id) > ($2, $3))
		ORDER BY created_at, comment_id
		LIMIT $4`, postId, after, cur.ID, limit+1)
	if err != nil {
		return CommentPage{}, err
	}
	type row struct {
		c  model.Comment
		at time.Time
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.c.CommentId, &x.c.ParentCommentId, &x.c.RootCommentId, &x.c.UserId, &x.c.PostId, &x.c.Depth, &x.c.Content, &x.at)
		x.c.CreatedAt = x.at.Unix()
		return x, err
	})
	if err != nil {
		return CommentPage{}, err
	}
	page := CommentPage{Comments: []model.Comment{}}
	for i, x := range got {
		if i == limit {
			last := got[limit-1]
			page.NextCursor = pagecursor.Encode(commentCursor{T: last.at.UnixMicro(), ID: last.c.CommentId})
			break
		}
		page.Comments = append(page.Comments, x.c)
	}
	return page, nil
}
