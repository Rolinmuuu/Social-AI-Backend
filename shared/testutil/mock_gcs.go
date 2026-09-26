package testutil

import (
	"fmt"
	"io"
	"sync"
)

// MockGCSBackend is an in-memory mock for GoogleCloudStorageBackendInterface.
type MockGCSBackend struct {
	mu        sync.Mutex
	Files     map[string][]byte
	SaveErr   error
	DeleteErr error
}

func NewMockGCSBackend() *MockGCSBackend {
	return &MockGCSBackend{Files: make(map[string][]byte)}
}

func (m *MockGCSBackend) SaveToGCS(r io.Reader, objectName string) (string, error) {
	if m.SaveErr != nil {
		return "", m.SaveErr
	}
	data, _ := io.ReadAll(r)
	m.mu.Lock()
	m.Files[objectName] = data
	m.mu.Unlock()
	return fmt.Sprintf("https://storage.googleapis.com/test-bucket/%s?signed=true", objectName), nil
}

func (m *MockGCSBackend) DeleteFromGCS(objectName string) error {
	if m.DeleteErr != nil {
		return m.DeleteErr
	}
	m.mu.Lock()
	delete(m.Files, objectName)
	m.mu.Unlock()
	return nil
}

// Count returns the number of stored objects.
func (m *MockGCSBackend) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Files)
}

func (m *MockGCSBackend) GenerateSignedURL(objectName string) (string, error) {
	return fmt.Sprintf("https://storage.googleapis.com/test-bucket/%s?signed=true&expires=3600", objectName), nil
}
