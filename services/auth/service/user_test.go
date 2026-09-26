package service

import (
	"context"
	"sync"
	"testing"

	"socialai/shared/db/dbtest"
	"socialai/shared/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func newTestUserService(t *testing.T) *UserService {
	return NewUserService(dbtest.New(t))
}

func TestAddUser_Success(t *testing.T) {
	svc := newTestUserService(t)
	ctx := context.Background()

	user := &model.User{UserId: "alice", Password: "secret123"}
	require.NoError(t, svc.AddUser(ctx, user))

	var stored string
	require.NoError(t, svc.db.QueryRow(ctx, `SELECT password_hash FROM users WHERE user_id = 'alice'`).Scan(&stored))
	assert.NotEqual(t, "secret123", stored, "password should be hashed")
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(stored), []byte("secret123")))
}

func TestAddUser_DuplicateUser(t *testing.T) {
	svc := newTestUserService(t)
	ctx := context.Background()
	require.NoError(t, svc.AddUser(ctx, &model.User{UserId: "alice", Password: "first"}))

	err := svc.AddUser(ctx, &model.User{UserId: "alice", Password: "newpass"})
	assert.ErrorIs(t, err, ErrUserAlreadyExisted)
	assert.NoError(t, svc.CheckUser(ctx, "alice", "first"), "the first user's password must be untouched")
}

// Ten sign-ups race for one id: exactly one wins, and it keeps its password.
func TestAddUser_ConcurrentSignupsOneWinner(t *testing.T) {
	svc := newTestUserService(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]error, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = svc.AddUser(ctx, &model.User{UserId: "bob", Password: string(rune('a' + i))})
		}(i)
	}
	wg.Wait()

	winner := -1
	for i, err := range results {
		if err == nil {
			require.Equal(t, -1, winner, "two sign-ups succeeded")
			winner = i
		} else {
			assert.ErrorIs(t, err, ErrUserAlreadyExisted)
		}
	}
	require.NotEqual(t, -1, winner)
	assert.NoError(t, svc.CheckUser(ctx, "bob", string(rune('a'+winner))))
}

func TestCheckUser_ValidCredentials(t *testing.T) {
	svc := newTestUserService(t)
	ctx := context.Background()
	require.NoError(t, svc.AddUser(ctx, &model.User{UserId: "alice", Password: "secret123"}))

	assert.NoError(t, svc.CheckUser(ctx, "alice", "secret123"))
}

func TestCheckUser_WrongPassword(t *testing.T) {
	svc := newTestUserService(t)
	ctx := context.Background()
	require.NoError(t, svc.AddUser(ctx, &model.User{UserId: "alice", Password: "secret123"}))

	assert.ErrorIs(t, svc.CheckUser(ctx, "alice", "wrongpass"), ErrInvalidCredentials)
}

func TestCheckUser_UserNotFound(t *testing.T) {
	svc := newTestUserService(t)
	assert.ErrorIs(t, svc.CheckUser(context.Background(), "nobody", "pass"), ErrInvalidCredentials)
}
