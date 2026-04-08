package testutil

import (
	"fmt"
	"io"
)

// MockGCSBackend is an in-memory mock for GoogleCloudStorageBackendInterface.
type MockGCSBackend struct {
	Files   map[string][]byte
	SaveErr error
}

func NewMockGCSBackend() *MockGCSBackend {
	return &MockGCSBackend{Files: make(map[string][]byte)}
}

func (m *MockGCSBackend) SaveToGCS(r io.Reader, objectName string) (string, error) {
	if m.SaveErr != nil {
		return "", m.SaveErr
	}
	data, _ := io.ReadAll(r)
	m.Files[objectName] = data
	return fmt.Sprintf("https://storage.googleapis.com/test-bucket/%s?signed=true", objectName), nil
}

func (m *MockGCSBackend) DeleteFromGCS(objectName string) error {
	delete(m.Files, objectName)
	return nil
}

func (m *MockGCSBackend) GenerateSignedURL(objectName string) (string, error) {
	return fmt.Sprintf("https://storage.googleapis.com/test-bucket/%s?signed=true&expires=3600", objectName), nil
}
