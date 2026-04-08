package testutil

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MockRedisBackend is an in-memory mock for RedisBackendInterface.
type MockRedisBackend struct {
	mu      sync.RWMutex
	store   map[string]string
	sets    map[string]map[string]bool
	lists   map[string][]string
	GetErr  error
}

func NewMockRedisBackend() *MockRedisBackend {
	return &MockRedisBackend{
		store: make(map[string]string),
		sets:  make(map[string]map[string]bool),
		lists: make(map[string][]string),
	}
}

func (m *MockRedisBackend) Set(_ context.Context, key string, value interface{}, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = value.(string)
	return nil
}

func (m *MockRedisBackend) Get(_ context.Context, key string) (string, error) {
	if m.GetErr != nil {
		return "", m.GetErr
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.store[key]
	if !ok {
		return "", errors.New("key not found")
	}
	return v, nil
}

func (m *MockRedisBackend) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.store, k)
	}
	return nil
}

func (m *MockRedisBackend) SAdd(_ context.Context, key string, members ...interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sets[key] == nil {
		m.sets[key] = make(map[string]bool)
	}
	for _, member := range members {
		m.sets[key][member.(string)] = true
	}
	return nil
}

func (m *MockRedisBackend) SIsMember(_ context.Context, key string, member interface{}) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.sets[key] == nil {
		return false, nil
	}
	return m.sets[key][member.(string)], nil
}

func (m *MockRedisBackend) LPush(_ context.Context, key string, values ...interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range values {
		var s string
		switch val := v.(type) {
		case string:
			s = val
		case []byte:
			s = string(val)
		}
		m.lists[key] = append([]string{s}, m.lists[key]...)
	}
	return nil
}

func (m *MockRedisBackend) LRange(_ context.Context, key string, start, stop int64) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.lists[key]
	if start >= int64(len(list)) {
		return nil, nil
	}
	end := stop + 1
	if end > int64(len(list)) {
		end = int64(len(list))
	}
	return list[start:end], nil
}

func (m *MockRedisBackend) LTrim(_ context.Context, key string, start, stop int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.lists[key]
	end := stop + 1
	if end > int64(len(list)) {
		end = int64(len(list))
	}
	if start >= int64(len(list)) {
		m.lists[key] = nil
		return nil
	}
	m.lists[key] = list[start:end]
	return nil
}

func (m *MockRedisBackend) Expire(_ context.Context, _ string, _ time.Duration) error {
	return nil
}

// GetList is a test helper to inspect Redis list contents.
func (m *MockRedisBackend) GetList(key string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lists[key]
}
