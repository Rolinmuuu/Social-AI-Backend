package service

import "errors"

var (
	ErrUserAlreadyExisted = errors.New("user already exists")
	ErrInvalidCredentials = errors.New("invalid credentials")
)
