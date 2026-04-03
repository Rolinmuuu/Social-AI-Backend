package backend

import (
	"context"
	"fmt"
	"io"

	"socialai/shared/constants"

	"cloud.google.com/go/storage"
)

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
	if err := object.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		return "", err
	}
	attrs, err := object.Attrs(ctx)
	if err != nil {
		return "", err
	}
	fmt.Printf("File uploaded to GCS: %s\n", attrs.MediaLink)
	return attrs.MediaLink, nil
}

func (b *GoogleCloudStorageBackend) DeleteFromGCS(objectName string) error {
	ctx := context.Background()
	return b.client.Bucket(b.bucket).Object(objectName).Delete(ctx)
}
