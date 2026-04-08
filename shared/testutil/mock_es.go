package testutil

import (
	"encoding/json"
	"fmt"

	"github.com/olivere/elastic/v7"
)

// MockESBackend is an in-memory mock for ElasticsearchBackendInterface.
type MockESBackend struct {
	Docs      map[string]map[string][]byte // index -> id -> JSON
	SaveErr   error
	ReadErr   error
	DeleteErr error
}

func NewMockESBackend() *MockESBackend {
	return &MockESBackend{Docs: make(map[string]map[string][]byte)}
}

func (m *MockESBackend) SaveToES(i interface{}, index string, id string) error {
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
	if m.ReadErr != nil {
		return nil, m.ReadErr
	}
	docs := m.Docs[index]
	var hits []*elastic.SearchHit
	for id, data := range docs {
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
	if m.Docs[index] != nil {
		delete(m.Docs[index], id)
	}
	return true, nil
}

func (m *MockESBackend) IncrementFieldInES(index, id, field string, value int) error {
	return nil
}

func (m *MockESBackend) KNNSearchFromES(index string, field string, vector []float32, k int) (*elastic.SearchResult, error) {
	return m.ReadFromES(nil, index)
}

// SetDoc is a test helper that puts a document directly into the mock store.
func (m *MockESBackend) SetDoc(index, id string, doc interface{}) {
	if m.Docs[index] == nil {
		m.Docs[index] = make(map[string][]byte)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("SetDoc marshal error: %v", err))
	}
	m.Docs[index][id] = data
}
