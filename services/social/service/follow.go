package service

import (
	"encoding/json"
	"fmt"
	"time"

	"socialai/shared/backend"
	"socialai/shared/constants"
	"socialai/shared/model"

	"github.com/google/uuid"
	elastic "github.com/olivere/elastic/v7"
)

// SocialService handles follow/follower relationships.
type SocialService struct {
	es backend.ElasticsearchBackendInterface
}

func NewSocialService(es backend.ElasticsearchBackendInterface) *SocialService {
	return &SocialService{es: es}
}

// AddFollow creates a follow relationship. Returns ErrAlreadyFollowing if it exists.
func (s *SocialService) AddFollow(followerId, followeeId string) (string, error) {
	if followerId == followeeId {
		return "", ErrCannotFollowSelf
	}

	existing := elastic.NewBoolQuery().
		Filter(elastic.NewTermQuery("follower_id", followerId)).
		Filter(elastic.NewTermQuery("followee_id", followeeId))
	result, err := s.es.ReadFromES(existing, constants.FOLLOW_INDEX)
	if err != nil {
		return "", fmt.Errorf("failed to check existing follow: %w", err)
	}
	if result.TotalHits() > 0 {
		return "", ErrAlreadyFollowing
	}

	follow := model.Follow{
		FollowId:   uuid.New().String(),
		FollowerId: followerId,
		FolloweeId: followeeId,
		CreatedAt:  time.Now(),
	}
	if err := s.es.SaveToES(follow, constants.FOLLOW_INDEX, follow.FollowId); err != nil {
		return "", fmt.Errorf("failed to save follow: %w", err)
	}
	return follow.FollowId, nil
}

// RemoveFollow deletes a follow relationship. Returns ErrNotFollowing if it doesn't exist.
func (s *SocialService) RemoveFollow(followerId, followeeId string) error {
	query := elastic.NewBoolQuery().
		Filter(elastic.NewTermQuery("follower_id", followerId)).
		Filter(elastic.NewTermQuery("followee_id", followeeId))
	result, err := s.es.ReadFromES(query, constants.FOLLOW_INDEX)
	if err != nil {
		return fmt.Errorf("failed to find follow: %w", err)
	}
	if result.TotalHits() == 0 {
		return ErrNotFollowing
	}

	followDocId := result.Hits.Hits[0].Id
	if _, err := s.es.DeleteFromES(constants.FOLLOW_INDEX, followDocId); err != nil {
		return fmt.Errorf("failed to delete follow: %w", err)
	}
	return nil
}

// GetFollowerIds returns the IDs of users who follow userId.
func (s *SocialService) GetFollowerIds(userId string) ([]string, error) {
	query := elastic.NewTermQuery("followee_id", userId)
	result, err := s.es.ReadFromES(query, constants.FOLLOW_INDEX)
	if err != nil {
		return nil, fmt.Errorf("failed to get followers: %w", err)
	}

	var ids []string
	for _, hit := range result.Hits.Hits {
		var follow model.Follow
		if err := json.Unmarshal(hit.Source, &follow); err == nil {
			ids = append(ids, follow.FollowerId)
		}
	}
	return ids, nil
}

// GetFollowingIds returns the IDs of users that userId follows.
func (s *SocialService) GetFollowingIds(userId string) ([]string, error) {
	query := elastic.NewTermQuery("follower_id", userId)
	result, err := s.es.ReadFromES(query, constants.FOLLOW_INDEX)
	if err != nil {
		return nil, fmt.Errorf("failed to get following: %w", err)
	}

	var ids []string
	for _, hit := range result.Hits.Hits {
		var follow model.Follow
		if err := json.Unmarshal(hit.Source, &follow); err == nil {
			ids = append(ids, follow.FolloweeId)
		}
	}
	return ids, nil
}
