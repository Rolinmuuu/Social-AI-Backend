package service

import (
	"encoding/json"
	"fmt"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/model"

	"golang.org/x/crypto/bcrypt"
	"github.com/olivere/elastic/v7"
)

type UserService struct {
	es backend.ElasticsearchBackendInterface
}

func NewUserService(es backend.ElasticsearchBackendInterface) *UserService {
	return &UserService{es: es}
}

// AddUser registers a new user. Returns ErrUserAlreadyExisted if the user_id is taken.
func (s *UserService) AddUser(user *model.User) error {
	query := elastic.NewTermQuery("user_id", user.UserId)
	result, err := s.es.ReadFromES(query, constants.USER_INDEX)
	if err != nil {
		return fmt.Errorf("failed to read user from ES: %w", err)
	}
	if result.TotalHits() > 0 {
		return ErrUserAlreadyExisted
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}
	user.Password = string(hashed)

	if err := s.es.SaveToES(user, constants.USER_INDEX, user.UserId); err != nil {
		return fmt.Errorf("failed to save user: %w", err)
	}
	return nil
}

// CheckUser verifies user_id and password against the stored bcrypt hash.
// Returns ErrInvalidCredentials if the credentials do not match.
func (s *UserService) CheckUser(userId, password string) error {
	query := elastic.NewTermQuery("user_id", userId)
	result, err := s.es.ReadFromES(query, constants.USER_INDEX)
	if err != nil {
		return fmt.Errorf("failed to read user from ES: %w", err)
	}

	for _, hit := range result.Hits.Hits {
		var stored model.User
		if err := json.Unmarshal(hit.Source, &stored); err != nil {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(stored.Password), []byte(password)) == nil {
			return nil
		}
	}
	return ErrInvalidCredentials
}
