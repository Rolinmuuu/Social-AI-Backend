package service

import "errors"

var (
	ErrPostNotFound    = errors.New("post not found")
	ErrAlreadyLiked    = errors.New("post already liked")
	ErrNotLiked        = errors.New("post not liked")
	ErrCommentNotFound = errors.New("comment not found")
	ErrInvalidComment  = errors.New("comment must be 1-2000 characters")
	ErrNotPostOwner    = errors.New("only the author can delete this post")
)
