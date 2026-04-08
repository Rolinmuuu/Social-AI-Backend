package service

import (
	"testing"

	"socialai/shared/model"
	"socialai/shared/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func newTestUserService() (*UserService, *testutil.MockESBackend) {
	es := testutil.NewMockESBackend()
	svc := NewUserService(es)
	return svc, es
}

// ──────────────────────── AddUser ────────────────────────

func TestAddUser_Success(t *testing.T) {
	svc, es := newTestUserService()

	user := &model.User{UserId: "alice", Password: "secret123"}
	err := svc.AddUser(user)

	require.NoError(t, err)
	assert.NotNil(t, es.Docs["user"]["alice"], "user should be saved to ES")
	assert.NotEqual(t, "secret123", user.Password, "password should be hashed")
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(user.Password), []byte("secret123")))
}

func TestAddUser_DuplicateUser(t *testing.T) {
	svc, es := newTestUserService()
	es.SetDoc("user", "alice", model.User{UserId: "alice", Password: "hashed"})

	user := &model.User{UserId: "alice", Password: "newpass"}
	err := svc.AddUser(user)

	assert.ErrorIs(t, err, ErrUserAlreadyExisted)
}

// ──────────────────────── CheckUser ────────────────────────

func TestCheckUser_ValidCredentials(t *testing.T) {
	svc, es := newTestUserService()
	hashed, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
	es.SetDoc("user", "alice", model.User{UserId: "alice", Password: string(hashed)})

	err := svc.CheckUser("alice", "secret123")
	assert.NoError(t, err)
}

func TestCheckUser_WrongPassword(t *testing.T) {
	svc, es := newTestUserService()
	hashed, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
	es.SetDoc("user", "alice", model.User{UserId: "alice", Password: string(hashed)})

	err := svc.CheckUser("alice", "wrongpass")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestCheckUser_UserNotFound(t *testing.T) {
	svc, _ := newTestUserService()

	err := svc.CheckUser("nobody", "pass")
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}
