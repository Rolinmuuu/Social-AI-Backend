package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"socialai/shared/constants"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

const signedURLExpiry = 1 * time.Hour

var GCSBackend GoogleCloudStorageBackendInterface

type GoogleCloudStorageBackend struct {
	client *storage.Client
	bucket string
	// publicBase, when set, is where objects are served from instead of a signed URL
	// (the local GCS emulator in docker compose cannot sign URLs).
	publicBase string
}

// InitGCSBackend connects to Cloud Storage with the default credentials. When
// STORAGE_EMULATOR_HOST is set (docker compose runs fake-gcs-server), the client talks to the
// emulator without credentials and the bucket is created if it does not exist yet, so the
// whole system starts on a machine that has no Google Cloud account.
func InitGCSBackend() (GoogleCloudStorageBackendInterface, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	b := &GoogleCloudStorageBackend{
		client:     client,
		bucket:     constants.GCS_BUCKET,
		publicBase: strings.TrimRight(os.Getenv("GCS_PUBLIC_BASE_URL"), "/"),
	}
	if os.Getenv("STORAGE_EMULATOR_HOST") != "" {
		err := client.Bucket(b.bucket).Create(ctx, "local-emulator", nil)
		var apiErr *googleapi.Error
		if err != nil && !(errors.As(err, &apiErr) && apiErr.Code == http.StatusConflict) {
			return nil, fmt.Errorf("create bucket %s in emulator: %w", b.bucket, err)
		}
	}
	return b, nil
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

	if b.publicBase != "" {
		return b.publicBase + "/" + b.bucket + "/" + objectName, nil
	}
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
