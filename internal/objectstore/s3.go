// Package objectstore provides provider-neutral immutable object publication.
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Evidence proves immutable object identity without exposing provider credentials.
type Evidence struct {
	SHA256    string
	SizeBytes int64
	ETag      string
}

// Store is the small provider-neutral durable object boundary.
type Store interface {
	PutImmutable(context.Context, string, io.Reader, int64, string) (Evidence, error)
	HeadVerified(context.Context, string, Evidence) (Evidence, error)
	GetVerified(context.Context, string, Evidence) (io.ReadCloser, Evidence, error)
	Delete(context.Context, string) error
}

// S3Config contains explicit S3-compatible authority.
type S3Config struct {
	Endpoint         string
	Region           string
	Bucket           string
	AccessKeyID      string
	SecretAccessKey  string
	UsePathStyle     bool
	RetryMaxAttempts int
	HTTPTimeout      time.Duration
	TempDirectory    string
	MaxObjectBytes   int64
}

// S3Store publishes immutable objects to one S3-compatible bucket.
type S3Store struct {
	client         *s3.Client
	bucket         string
	tempDirectory  string
	maxObjectBytes int64
}

// NewS3Store constructs an S3-compatible immutable object store.
func NewS3Store(ctx context.Context, objectConfig S3Config) (*S3Store, error) {
	if objectConfig.Endpoint == "" || objectConfig.Region == "" || objectConfig.Bucket == "" ||
		objectConfig.AccessKeyID == "" || objectConfig.SecretAccessKey == "" ||
		objectConfig.RetryMaxAttempts < 1 || objectConfig.HTTPTimeout <= 0 ||
		objectConfig.TempDirectory == "" || objectConfig.MaxObjectBytes < 1 {
		return nil, errors.New("SecondBox S3 authority, retry, timeout, temporary directory, and object limit are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox S3 construction context failed: %w", err)
	}
	endpointURL, err := url.Parse(objectConfig.Endpoint)
	if err != nil || !endpointURL.IsAbs() ||
		(endpointURL.Scheme != "http" && endpointURL.Scheme != "https") || endpointURL.Host == "" {
		return nil, errors.New("SecondBox S3 endpoint must be an absolute HTTP or HTTPS URL")
	}
	tempDirectoryInfo, err := os.Stat(objectConfig.TempDirectory)
	if err != nil {
		return nil, fmt.Errorf("SecondBox S3 temporary directory is unavailable: %w", err)
	}
	if !tempDirectoryInfo.IsDir() {
		return nil, errors.New("SecondBox S3 temporary directory path is not a directory")
	}
	awsConfig := aws.Config{
		Region: objectConfig.Region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			objectConfig.AccessKeyID, objectConfig.SecretAccessKey, "",
		)),
		HTTPClient: &http.Client{Timeout: objectConfig.HTTPTimeout},
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(options *retry.StandardOptions) {
				options.MaxAttempts = objectConfig.RetryMaxAttempts
			})
		},
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(objectConfig.Endpoint)
		options.UsePathStyle = objectConfig.UsePathStyle
	})
	return &S3Store{
		client: client, bucket: objectConfig.Bucket,
		tempDirectory: objectConfig.TempDirectory, maxObjectBytes: objectConfig.MaxObjectBytes,
	}, nil
}

// PutImmutable validates bytes before upload and verifies provider evidence afterwards.
func (store *S3Store) PutImmutable(
	ctx context.Context,
	key string,
	reader io.Reader,
	sizeBytes int64,
	expectedSHA256 string,
) (evidence Evidence, resultErr error) {
	if key == "" || sizeBytes < 0 || sizeBytes > store.maxObjectBytes || !sha256Pattern.MatchString(expectedSHA256) {
		return Evidence{}, errors.New("SecondBox immutable object key, size, or SHA-256 is invalid")
	}
	staging, err := os.CreateTemp(store.tempDirectory, "secondbox-object-upload-*")
	if err != nil {
		return Evidence{}, fmt.Errorf("SecondBox immutable upload staging create failed: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeAndRemove(staging))
	}()
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(staging, hasher), io.LimitReader(reader, sizeBytes+1))
	if err != nil {
		return Evidence{}, fmt.Errorf("SecondBox immutable object staging failed: %w", err)
	}
	if written != sizeBytes {
		return Evidence{}, errors.New("SecondBox immutable object size does not match declared evidence")
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA256 != expectedSHA256 {
		return Evidence{}, errors.New("SecondBox immutable object SHA-256 does not match declared evidence")
	}
	if _, err := staging.Seek(0, io.SeekStart); err != nil {
		return Evidence{}, fmt.Errorf("SecondBox immutable upload staging rewind failed: %w", err)
	}
	if _, err := store.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key), Body: staging,
		ContentLength: aws.Int64(sizeBytes), IfNoneMatch: aws.String("*"),
		Metadata: map[string]string{"sha256": expectedSHA256},
	}); err != nil {
		var apiError smithy.APIError
		if errors.As(err, &apiError) && apiError.ErrorCode() == "PreconditionFailed" {
			body, evidence, verifyErr := store.GetVerified(ctx, key, Evidence{
				SHA256: expectedSHA256, SizeBytes: sizeBytes,
			})
			if verifyErr != nil {
				return Evidence{}, verifyErr
			}
			if closeErr := body.Close(); closeErr != nil {
				return Evidence{}, closeErr
			}
			return evidence, nil
		}
		return Evidence{}, fmt.Errorf("SecondBox immutable object upload failed: %w", err)
	}
	expected := Evidence{SHA256: expectedSHA256, SizeBytes: sizeBytes}
	if _, err := store.HeadVerified(ctx, key, expected); err != nil {
		return Evidence{}, err
	}
	verifiedBody, verified, err := store.GetVerified(ctx, key, expected)
	if err != nil {
		return Evidence{}, err
	}
	if err := verifiedBody.Close(); err != nil {
		return Evidence{}, err
	}
	return verified, nil
}

// HeadVerified proves that published provider metadata matches database evidence.
func (store *S3Store) HeadVerified(ctx context.Context, key string, expected Evidence) (Evidence, error) {
	if err := store.validateEvidence(key, expected); err != nil {
		return Evidence{}, err
	}
	output, err := store.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key),
	})
	if err != nil {
		return Evidence{}, fmt.Errorf("SecondBox immutable object verification failed: %w", err)
	}
	actual := Evidence{SHA256: output.Metadata["sha256"], SizeBytes: aws.ToInt64(output.ContentLength), ETag: aws.ToString(output.ETag)}
	if actual.SHA256 != expected.SHA256 || actual.SizeBytes != expected.SizeBytes {
		return Evidence{}, errors.New("SecondBox immutable object provider evidence does not match published metadata")
	}
	return actual, nil
}

// GetVerified buffers and verifies immutable bytes before exposing them to restore.
func (store *S3Store) GetVerified(
	ctx context.Context,
	key string,
	expected Evidence,
) (_ io.ReadCloser, evidence Evidence, resultErr error) {
	if err := store.validateEvidence(key, expected); err != nil {
		return nil, Evidence{}, err
	}
	if _, err := store.HeadVerified(ctx, key, expected); err != nil {
		return nil, Evidence{}, err
	}
	output, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, Evidence{}, fmt.Errorf("SecondBox immutable object download failed: %w", err)
	}
	staging, err := os.CreateTemp(store.tempDirectory, "secondbox-object-download-*")
	if err != nil {
		bodyCloseErr := output.Body.Close()
		return nil, Evidence{}, errors.Join(
			fmt.Errorf("SecondBox immutable download staging create failed: %w", err),
			bodyCloseErr,
		)
	}
	cleanup := true
	defer func() {
		if cleanup {
			resultErr = errors.Join(resultErr, closeAndRemove(staging))
		}
	}()
	hasher := sha256.New()
	written, copyErr := io.Copy(
		io.MultiWriter(staging, hasher),
		io.LimitReader(output.Body, expected.SizeBytes+1),
	)
	bodyCloseErr := output.Body.Close()
	if copyErr != nil {
		return nil, Evidence{}, errors.Join(
			fmt.Errorf("SecondBox immutable object download staging failed: %w", copyErr), bodyCloseErr,
		)
	}
	if bodyCloseErr != nil {
		return nil, Evidence{}, fmt.Errorf("SecondBox immutable object download close failed: %w", bodyCloseErr)
	}
	actual := Evidence{SHA256: hex.EncodeToString(hasher.Sum(nil)), SizeBytes: written, ETag: aws.ToString(output.ETag)}
	if actual.SHA256 != expected.SHA256 || actual.SizeBytes != expected.SizeBytes {
		return nil, Evidence{}, errors.New("SecondBox immutable object bytes failed integrity verification")
	}
	if _, err := staging.Seek(0, io.SeekStart); err != nil {
		return nil, Evidence{}, fmt.Errorf("SecondBox immutable download staging rewind failed: %w", err)
	}
	cleanup = false
	return &removingReadCloser{File: staging}, actual, nil
}

func (store *S3Store) validateEvidence(key string, evidence Evidence) error {
	if key == "" || evidence.SizeBytes < 0 || evidence.SizeBytes > store.maxObjectBytes ||
		!sha256Pattern.MatchString(evidence.SHA256) {
		return errors.New("SecondBox immutable object key or evidence is invalid")
	}
	return nil
}

type removingReadCloser struct {
	*os.File
}

func (reader *removingReadCloser) Close() error {
	return closeAndRemove(reader.File)
}

func closeAndRemove(file *os.File) error {
	path := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if closeErr != nil {
		closeErr = fmt.Errorf("SecondBox object staging close failed: %w", closeErr)
	}
	if removeErr != nil {
		removeErr = fmt.Errorf("SecondBox object staging removal failed: %w", removeErr)
	}
	return errors.Join(closeErr, removeErr)
}

// Delete removes an unreachable immutable object during garbage collection.
func (store *S3Store) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("SecondBox immutable object key is required")
	}
	if _, err := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(store.bucket), Key: aws.String(key),
	}); err != nil {
		return fmt.Errorf("SecondBox immutable object delete failed: %w", err)
	}
	return nil
}
