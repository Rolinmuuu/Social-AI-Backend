package service

import "errors"

var (
	ErrCannotMessageSelf    = errors.New("cannot send message to yourself")
	ErrInvalidContent       = errors.New("content must be 1-4000 characters")
	ErrUserNotFound         = errors.New("user not found")
	ErrKeyReused            = errors.New("Idempotency-Key was used for a different message")
	ErrConversationNotFound = errors.New("conversation not found")
)
