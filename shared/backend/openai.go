package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"socialai/shared/constants"
	openai "github.com/sashabaranov/go-openai"
)

type OpenAIBackend struct {
	client *openai.Client
}

type OpenAIBackendInterface interface {
	GenerateImage(ctx context.Context, prompt string) (string, error)
	GetEmbedding(ctx context.Context, text string) ([]float32, error)
}

func InitOpenAIBackend() (OpenAIBackendInterface, error) {
	apiKey := constants.OPENAI_API_KEY
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}
	client := openai.NewClient(apiKey)
	return &OpenAIBackend{client: client}, nil
}

// GenerateImage call DALL-E API to generate an image
// and return the image URL
func (b *OpenAIBackend) GenerateImage(ctx context.Context, prompt string) (string, error) {
	response, err := b.client.CreateImage(ctx, openai.ImageRequest{
		Prompt: prompt,
		Model: openai.CreateImageModelDallE3,
		N: 1,
		Size: openai.CreateImageSize1024x1024,
		Quality: openai.CreateImageQualityStandard,
		ResponseFormat: openai.CreateImageResponseFormatURL,
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate image: %w", err)
	}
	if len(response.Data) == 0 {
		return "", fmt.Errorf("no image generated")
	}
	return response.Data[0].URL, nil
}

// GetEmbedding call OpenAI API to get the embedding of the text
// and return the embedding vector
func (b *OpenAIBackend) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	response, err := b.client.CreateEmbeddings(ctx, openai.EmbeddingRequestStrings{
		Model: openai.SmallEmbedding3,
		Input: []string{text},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get embedding: %w", err)
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("no embedding generated")
	}
	return response.Data[0].Embedding, nil
}

// DownloadImage download the image from the URL 
// and return the image bytes as io.ReadCloser
func DownloadImage(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("failed to download image: %s", resp.Status)
	}
	return resp.Body, nil
}