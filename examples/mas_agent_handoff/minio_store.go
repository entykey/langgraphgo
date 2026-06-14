package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

var minioClient *minio.Client
var minioBucket string

// initMinio connects to MinIO using MINIO_* env vars and configures the bucket.
// Falls back silently to in-memory store when vars are absent.
func initMinio() {
	endpoint := os.Getenv("MINIO_ENDPOINT")
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	bucket := os.Getenv("MINIO_BUCKET")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		fmt.Println("[minio] disabled — set MINIO_ENDPOINT, MINIO_ACCESS_KEY, MINIO_SECRET_KEY to enable")
		return
	}
	if bucket == "" {
		bucket = "mas-images"
	}

	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: os.Getenv("MINIO_USE_SSL") == "1",
	})
	if err != nil {
		fmt.Printf("[minio] connect error: %v — falling back to memory\n", err)
		return
	}

	ctx := context.Background()
	exists, err := mc.BucketExists(ctx, bucket)
	if err != nil {
		fmt.Printf("[minio] bucket check error: %v — falling back to memory\n", err)
		return
	}
	if !exists {
		if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			fmt.Printf("[minio] create bucket %q error: %v — falling back to memory\n", bucket, err)
			return
		}
		fmt.Printf("[minio] created bucket %q\n", bucket)
	}

	minioClient = mc
	minioBucket = bucket

	ttlDays := 30
	if v := os.Getenv("MINIO_IMAGE_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttlDays = n
		}
	}
	setMinioLifecycle(mc, bucket, ttlDays)

	fmt.Printf("[minio] ready → %s / %s\n", endpoint, bucket)
}

// setMinioLifecycle installs an expiry rule so objects are automatically deleted after ttlDays.
func setMinioLifecycle(mc *minio.Client, bucket string, ttlDays int) {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = []lifecycle.Rule{
		{
			ID:     "expire-images",
			Status: "Enabled",
			Expiration: lifecycle.Expiration{
				Days: lifecycle.ExpirationDays(ttlDays),
			},
		},
	}
	if err := mc.SetBucketLifecycle(context.Background(), bucket, cfg); err != nil {
		fmt.Printf("[minio] lifecycle policy error: %v\n", err)
		return
	}
	fmt.Printf("[minio] lifecycle: objects expire after %d days (override: MINIO_IMAGE_TTL_DAYS)\n", ttlDays)
}

func minioEnabled() bool { return minioClient != nil }

// minioPut stores raw image bytes in MinIO.
func minioPut(id, mime string, data []byte) error {
	_, err := minioClient.PutObject(
		context.Background(), minioBucket, id,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: mime},
	)
	return err
}

// minioPresignURL generates a presigned GET URL valid for the given duration.
// The browser can fetch the image directly from MinIO — Go is not in the data path.
func minioPresignURL(id string, expiry time.Duration) (string, error) {
	u, err := minioClient.PresignedGetObject(
		context.Background(), minioBucket, id, expiry, url.Values{},
	)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// minioStream streams an object directly to the ResponseWriter (fallback when presign fails).
func minioStream(w http.ResponseWriter, id string) bool {
	obj, err := minioClient.GetObject(context.Background(), minioBucket, id, minio.GetObjectOptions{})
	if err != nil {
		return false
	}
	defer obj.Close()
	stat, err := obj.Stat()
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", stat.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size))
	io.Copy(w, obj) //nolint:errcheck
	return true
}

// minioGetBytes fetches raw bytes from MinIO (used for LLM base64 encoding).
func minioGetBytes(id string) (data []byte, mime string, err error) {
	obj, getErr := minioClient.GetObject(context.Background(), minioBucket, id, minio.GetObjectOptions{})
	if getErr != nil {
		return nil, "", getErr
	}
	defer obj.Close()
	stat, statErr := obj.Stat()
	if statErr != nil {
		return nil, "", statErr
	}
	data, err = io.ReadAll(obj)
	return data, stat.ContentType, err
}
