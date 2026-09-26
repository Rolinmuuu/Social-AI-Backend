package service

import (
	"context"
	"errors"
	"fmt"

	"socialai/shared/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// UserService owns the users table.
type UserService struct {
	db *pgxpool.Pool
}

func NewUserService(pool *pgxpool.Pool) *UserService {
	return &UserService{db: pool}
}

// dummyHash is compared against when the user does not exist, so "no such user" costs the
// same bcrypt time as "wrong password" and response times do not reveal which ids exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

// AddUser registers a new user. Returns ErrUserAlreadyExisted if the user_id is taken.
//
// The primary key is the uniqueness check. The Elasticsearch version searched for the id and
// then wrote the document, so two concurrent sign-ups with one id could both pass the search
// and the second silently replaced the first user's password.
func (s *UserService) AddUser(ctx context.Context, user *model.User) error {
	hashed, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}
	tag, err := s.db.Exec(ctx, `
		INSERT INTO users (user_id, username, password_hash, age, gender)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO NOTHING`,
		user.UserId, user.Username, string(hashed), user.Age, user.Gender)
	if err != nil {
		return fmt.Errorf("failed to save user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserAlreadyExisted
	}
	user.Password = string(hashed)
	return nil
}

// CheckUser verifies user_id and password against the stored bcrypt hash.
// Returns ErrInvalidCredentials if the credentials do not match.
func (s *UserService) CheckUser(ctx context.Context, userId, password string) error {
	var hash string
	err := s.db.QueryRow(ctx, `SELECT password_hash FROM users WHERE user_id = $1`, userId).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return ErrInvalidCredentials
	}
	if err != nil {
		return fmt.Errorf("failed to read user: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	return nil
}
