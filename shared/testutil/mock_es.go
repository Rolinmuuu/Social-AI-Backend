package testutil

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olivere/elastic/v7"
)

// MockESBackend is an in-memory mock for ElasticsearchBackendInterface.
type MockESBackend struct {
	Docs      map[string]map[string][]byte // index -> id -> JSON
	SaveErr   error
	ReadErr   error
	DeleteErr error
	// Increments records IncrementFieldInES calls: "index/id/field" -> total.
	Increments map[string]int
	// Queries counts read calls, to assert that caches absorb load (read with atomic).
	Queries int64
	// ReadDelay simulates query latency.
	ReadDelay time.Duration

	mu sync.RWMutex
}

func NewMockESBackend() *MockESBackend {
	return &MockESBackend{Docs: make(map[string]map[string][]byte)}
}

func (m *MockESBackend) SaveToES(i interface{}, index string, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.save(i, index, id)
}

func (m *MockESBackend) save(i interface{}, index string, id string) error {
	if m.SaveErr != nil {
		return m.SaveErr
	}
	if m.Docs[index] == nil {
		m.Docs[index] = make(map[string][]byte)
	}
	data, _ := json.Marshal(i)
	m.Docs[index][id] = data
	return nil
}

func (m *MockESBackend) ReadFromES(query elastic.Query, index string) (*elastic.SearchResult, error) {
	return m.ReadFromESWithSize(query, index, 10)
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
	docs := m.Docs[index]
	var hits []*elastic.SearchHit
	for id, data := range docs {
		var doc map[string]interface{}
		_ = json.Unmarshal(data, &doc)
		if !matches(query, doc) {
			continue
		}
		rawMsg := json.RawMessage(data)
		hits = append(hits, &elastic.SearchHit{
			Id:     id,
			Index:  index,
			Source: rawMsg,
		})
	}
	if size > 0 && len(hits) > size {
		hits = hits[:size]
	}
	totalHits := &elastic.TotalHits{Value: int64(len(hits)), Relation: "eq"}
	return &elastic.SearchResult{
		Hits: &elastic.SearchHits{
			TotalHits: totalHits,
			Hits:      hits,
		},
	}, nil
}

func (m *MockESBackend) DeleteFromES(index string, id string) (bool, error) {
	if m.DeleteErr != nil {
		return false, m.DeleteErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Docs[index] != nil {
		delete(m.Docs[index], id)
	}
	return true, nil
}

func (m *MockESBackend) IncrementFieldInES(index, id, field string, value int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Increments == nil {
		m.Increments = make(map[string]int)
	}
	m.Increments[index+"/"+id+"/"+field] += value
	return nil
}

// SearchSorted filters like the other mock reads and honours sort (with the post_id
// tie-break) and size.
func (m *MockESBackend) SearchSorted(query elastic.Query, index, sortField string, ascending bool, size int) (*elastic.SearchResult, error) {
	res, err := m.ReadFromESWithSize(query, index, 0)
	if err != nil {
		return nil, err
	}
	hits := res.Hits.Hits
	type sortKey struct {
		v       float64
		missing bool
		id      string
	}
	key := func(h *elastic.SearchHit) sortKey {
		var doc map[string]interface{}
		_ = json.Unmarshal(h.Source, &doc)
		v, ok := doc[sortField].(float64)
		id, _ := doc["post_id"].(string)
		return sortKey{v: v, missing: !ok, id: id}
	}
	// Like the real backend: sort by sortField, ties broken by post_id in the same direction,
	// documents without sortField last.
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := key(hits[i]), key(hits[j])
		if a.missing != b.missing {
			return !a.missing
		}
		if a.v != b.v {
			if ascending {
				return a.v < b.v
			}
			return a.v > b.v
		}
		if ascending {
			return a.id < b.id
		}
		return a.id > b.id
	})
	if size > 0 && len(hits) > size {
		hits = hits[:size]
	}
	res.Hits.Hits = hits
	return res, nil
}

// CreateInES fails with created=false when the id already exists, like op_type=create.
func (m *MockESBackend) CreateInES(i interface{}, index string, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SaveErr != nil {
		return false, m.SaveErr
	}
	if _, exists := m.Docs[index][id]; exists {
		return false, nil
	}
	return true, m.save(i, index, id)
}

// UpdateFieldsInES merges fields into the stored JSON document.
func (m *MockESBackend) UpdateFieldsInES(index string, id string, fields map[string]interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SaveErr != nil {
		return m.SaveErr
	}
	raw, ok := m.Docs[index][id]
	if !ok {
		return fmt.Errorf("document %s/%s not found", index, id)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	for k, v := range fields {
		doc[k] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	m.Docs[index][id] = data
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

func (m *MockESBackend) KNNSearchFromES(index string, field string, vector []float32, k int) (*elastic.SearchResult, error) {
	return m.ReadFromES(nil, index)
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
