package service

import "errors"

var (
	ErrPostNotFound    = errors.New("post not found")
	ErrAlreadyLiked    = errors.New("post already liked")
	ErrCommentNotFound = errors.New("comment not found")
	ErrNotPostOwner    = errors.New("only the author can delete this post")
)
