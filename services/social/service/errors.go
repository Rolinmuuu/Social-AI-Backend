package service

import "errors"

var (
	ErrCannotFollowSelf  = errors.New("cannot follow yourself")
	ErrAlreadyFollowing  = errors.New("already following this user")
	ErrNotFollowing      = errors.New("not following this user")
)
