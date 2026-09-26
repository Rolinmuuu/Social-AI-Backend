package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olivere/elastic/v7"
)

// MockESBackend is an in-memory ElasticsearchBackendInterface. It evaluates the
// bool/term/terms/range/exists filters the services build (see es_query.go) and implements
// external versioning like Elasticsearch: a write with a version not above the stored one
// is rejected (applied=false).
type MockESBackend struct {
	Docs     map[string]map[string][]byte // index -> id -> JSON
	Versions map[string]map[string]int64  // index -> id -> external version
	SaveErr  error
	ReadErr  error
	// Queries counts read calls (read with atomic).
	Queries int64
	// ReadDelay simulates query latency.
	ReadDelay time.Duration

	mu sync.RWMutex
}

func NewMockESBackend() *MockESBackend {
	return &MockESBackend{Docs: map[string]map[string][]byte{}, Versions: map[string]map[string]int64{}}
}

func (m *MockESBackend) ReadFromESWithSize(query elastic.Query, index string, size int) (*elastic.SearchResult, error) {
	atomic.AddInt64(&m.Queries, 1)
	if m.ReadDelay > 0 {
		time.Sleep(m.ReadDelay)
	}
	if m.ReadErr != nil {
		return nil, m.ReadErr
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.Docs[index]))
	for id := range m.Docs[index] {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic "relevance" order for tests
	var hits []*elastic.SearchHit
	for _, id := range ids {
		data := m.Docs[index][id]
		var doc map[string]interface{}
		_ = json.Unmarshal(data, &doc)
		if !matches(query, doc) {
			continue
		}
		hits = append(hits, &elastic.SearchHit{Id: id, Index: index, Source: json.RawMessage(data)})
	}
	if size > 0 && len(hits) > size {
		hits = hits[:size]
	}
	return &elastic.SearchResult{Hits: &elastic.SearchHits{
		TotalHits: &elastic.TotalHits{Value: int64(len(hits)), Relation: "eq"},
		Hits:      hits,
	}}, nil
}

// KNNSearchFromES ignores the vector (no similarity maths) but applies the filter and k.
func (m *MockESBackend) KNNSearchFromES(index, field string, vector []float32, k int, filter elastic.Query) (*elastic.SearchResult, error) {
	return m.ReadFromESWithSize(filter, index, k)
}

func (m *MockESBackend) IndexVersioned(index, id string, doc interface{}, version int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SaveErr != nil {
		return false, m.SaveErr
	}
	if m.Versions[index] == nil {
		m.Versions[index] = map[string]int64{}
	}
	if cur, ok := m.Versions[index][id]; ok && version <= cur {
		return false, nil
	}
	if m.Docs[index] == nil {
		m.Docs[index] = map[string][]byte{}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return false, err
	}
	m.Docs[index][id] = data
	m.Versions[index][id] = version
	return true, nil
}

func (m *MockESBackend) Scan(_ context.Context, index string, fn func(id string, source json.RawMessage) error) error {
	if m.ReadErr != nil {
		return m.ReadErr
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.Docs[index]))
	for id := range m.Docs[index] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	docs := make([][]byte, len(ids))
	for i, id := range ids {
		docs[i] = m.Docs[index][id]
	}
	m.mu.RUnlock()
	for i, id := range ids {
		if err := fn(id, json.RawMessage(docs[i])); err != nil {
			return err
		}
	}
	return nil
}

// Doc decodes a stored document into out (test helper).
func (m *MockESBackend) Doc(index, id string, out interface{}) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	raw, ok := m.Docs[index][id]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

// Version returns the stored external version of a document (0 if absent).
func (m *MockESBackend) Version(index, id string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Versions[index][id]
}

// SetDoc is a test helper that puts a document directly into the mock store.
func (m *MockESBackend) SetDoc(index, id string, doc interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Docs[index] == nil {
		m.Docs[index] = make(map[string][]byte)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("SetDoc marshal error: %v", err))
	}
	m.Docs[index][id] = data
}
