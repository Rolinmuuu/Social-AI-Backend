package testutil

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"socialai/shared/backend"
	"socialai/shared/feedplan"
)

// MockRedisBackend is an in-memory mock for RedisBackendInterface.
type MockRedisBackend struct {
	mu     sync.RWMutex
	store  map[string]string
	sets   map[string]map[string]bool
	lists  map[string][]string
	zsets  map[string]map[string]float64
	GetErr error
	// PipelineCalls counts AddToFeeds round trips.
	PipelineCalls int
	// FeedReads counts FeedItems calls.
	FeedReads int
}

func NewMockRedisBackend() *MockRedisBackend {
	return &MockRedisBackend{
		store: make(map[string]string),
		sets:  make(map[string]map[string]bool),
		lists: make(map[string][]string),
		zsets: make(map[string]map[string]float64),
	}
}

func (m *MockRedisBackend) Set(_ context.Context, key string, value interface{}, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch v := value.(type) {
	case string:
		m.store[key] = v
	case []byte:
		m.store[key] = string(v)
	default:
		m.store[key] = fmt.Sprintf("%v", v)
	}
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

func (m *MockRedisBackend) SetNX(_ context.Context, key string, value interface{}, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.store[key]; ok {
		return false, nil
	}
	m.store[key] = fmt.Sprintf("%v", value)
	return true, nil
}

func (m *MockRedisBackend) SAddCount(_ context.Context, key string, members ...interface{}) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sets[key] == nil {
		m.sets[key] = make(map[string]bool)
	}
	var added int64
	for _, member := range members {
		s := fmt.Sprintf("%v", member)
		if !m.sets[key][s] {
			m.sets[key][s] = true
			added++
		}
	}
	return added, nil
}

func (m *MockRedisBackend) SRem(_ context.Context, key string, members ...interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, member := range members {
		delete(m.sets[key], fmt.Sprintf("%v", member))
	}
	return nil
}

func (m *MockRedisBackend) SMembers(_ context.Context, key string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for k := range m.sets[key] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (m *MockRedisBackend) SPopN(_ context.Context, key string, count int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.sets[key] {
		if int64(len(out)) >= count {
			break
		}
		out = append(out, k)
		delete(m.sets[key], k)
	}
	return out, nil
}

func (m *MockRedisBackend) IncrBy(_ context.Context, key string, value int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, _ := strconv.ParseInt(m.store[key], 10, 64)
	cur += value
	m.store[key] = strconv.FormatInt(cur, 10)
	return cur, nil
}

func (m *MockRedisBackend) GetDel(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.store[key]
	if !ok {
		return "", nil // like the real backend: a missing key is not an error
	}
	delete(m.store, key)
	return v, nil
}

func (m *MockRedisBackend) ZRevRange(_ context.Context, key string, start, stop int64) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type kv struct {
		k string
		s float64
	}
	var all []kv
	for k, s := range m.zsets[key] {
		all = append(all, kv{k, s})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		return all[i].k > all[j].k
	})
	var out []string
	for i, e := range all {
		if int64(i) >= start && (stop < 0 || int64(i) <= stop) {
			out = append(out, e.k)
		}
	}
	return out, nil
}

// FeedItems mirrors the real backend: newest first, strictly after the cursor.
func (m *MockRedisBackend) FeedItems(_ context.Context, key string, after feedplan.Cursor, count int) ([]feedplan.Item, error) {
	m.mu.Lock()
	m.FeedReads++
	var items []feedplan.Item
	for k, s := range m.zsets[key] {
		items = append(items, feedplan.Item{PostID: k, CreatedAt: int64(s)})
	}
	m.mu.Unlock()
	items = feedplan.Before(feedplan.Merge(0, items), after)
	if len(items) > count {
		items = items[:count]
	}
	return items, nil
}

func (m *MockRedisBackend) AddToFeeds(_ context.Context, followerIDs []string, item feedplan.Item, maxLen int, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PipelineCalls++
	for _, id := range followerIDs {
		key := backend.HomeFeedKey(id)
		if m.zsets[key] == nil {
			m.zsets[key] = make(map[string]float64)
		}
		m.zsets[key][item.PostID] = float64(item.CreatedAt)
		for len(m.zsets[key]) > maxLen {
			oldest, first := "", true
			for k, s := range m.zsets[key] {
				if first || s < m.zsets[key][oldest] {
					oldest, first = k, false
				}
			}
			delete(m.zsets[key], oldest)
		}
	}
	return nil
}

// Feed returns a user's materialised home feed, newest first.
func (m *MockRedisBackend) Feed(userID string) []string {
	out, _ := m.ZRevRange(context.Background(), backend.HomeFeedKey(userID), 0, -1)
	return out
}

// IsMember is a test helper for sets.
func (m *MockRedisBackend) IsMember(key, member string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sets[key][member]
}
