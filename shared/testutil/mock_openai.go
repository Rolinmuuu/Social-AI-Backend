package testutil

import "context"

// MockOpenAIBackend is a mock for OpenAIBackendInterface.
type MockOpenAIBackend struct {
	ImageURL     string
	Embedding    []float32
	GenerateErr  error
	EmbeddingErr error
}

func NewMockOpenAIBackend() *MockOpenAIBackend {
	return &MockOpenAIBackend{
		ImageURL:  "https://openai.com/generated/test.png",
		Embedding: []float32{0.1, 0.2, 0.3},
	}
}

func (m *MockOpenAIBackend) GenerateImage(_ context.Context, _ string) (string, error) {
	if m.GenerateErr != nil {
		return "", m.GenerateErr
	}
	return m.ImageURL, nil
}

func (m *MockOpenAIBackend) GetEmbedding(_ context.Context, _ string) ([]float32, error) {
	if m.EmbeddingErr != nil {
		return nil, m.EmbeddingErr
	}
	return m.Embedding, nil
}
