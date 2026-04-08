package backend

import (
	"context"
	"fmt"
	"io"
	"time"

	"socialai/shared/constants"

	"cloud.google.com/go/storage"
)

const signedURLExpiry = 1 * time.Hour

var GCSBackend GoogleCloudStorageBackendInterface

type GoogleCloudStorageBackend struct {
	client *storage.Client
	bucket string
}

func InitGCSBackend() (GoogleCloudStorageBackendInterface, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &GoogleCloudStorageBackend{
		client: client,
		bucket: constants.GCS_BUCKET,
	}, nil
}

// SaveToGCS uploads a file to GCS as a private object (no public ACL).
// Returns the object name (not a public URL) — use GenerateSignedURL to get temporary access.
func (b *GoogleCloudStorageBackend) SaveToGCS(r io.Reader, objectName string) (string, error) {
	ctx := context.Background()
	object := b.client.Bucket(b.bucket).Object(objectName)
	writer := object.NewWriter(ctx)

	if _, err := io.Copy(writer, r); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	fmt.Printf("File uploaded to GCS: %s/%s\n", b.bucket, objectName)

	url, err := b.GenerateSignedURL(objectName)
	if err != nil {
		return fmt.Sprintf("https://storage.googleapis.com/%s/%s", b.bucket, objectName), nil
	}
	return url, nil
}

func (b *GoogleCloudStorageBackend) DeleteFromGCS(objectName string) error {
	ctx := context.Background()
	return b.client.Bucket(b.bucket).Object(objectName).Delete(ctx)
}

// GenerateSignedURL creates a time-limited signed URL for private GCS objects.
func (b *GoogleCloudStorageBackend) GenerateSignedURL(objectName string) (string, error) {
	url, err := b.client.Bucket(b.bucket).SignedURL(objectName, &storage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(signedURLExpiry),
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate signed URL: %w", err)
	}
	return url, nil
}
