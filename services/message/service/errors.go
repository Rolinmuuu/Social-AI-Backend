package service

import "errors"

var (
	ErrCannotMessageSelf = errors.New("cannot send message to yourself")
)
