// Package workspacearchive issues narrowly scoped trusted archive transfers.
package workspacearchive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type TransferProvider interface {
	Transfer(context.Context, string, string, *runnerv1.WorkspaceArchiveDescriptor) (*runnerv1.WorkspaceArchiveTransfer, error)
	Delete(context.Context, string) error
}

type ObjectStoreConfig struct {
	Bucket                  string           `json:"bucket"`
	Region                  string           `json:"region"`
	Endpoint                string           `json:"endpoint"`
	UsePathStyle            bool             `json:"usePathStyle"`
	TransferLifetimeSeconds int64            `json:"transferLifetimeSeconds"`
	ProfileIdleSeconds      map[string]int64 `json:"profileIdleSeconds"`
}

type S3TransferProvider struct {
	client      *s3.Client
	presigner   *s3.PresignClient
	bucket      string
	lifetime    time.Duration
	credentials aws.CredentialsProvider
	region      string
}

func NewS3TransferProvider(ctx context.Context, settings ObjectStoreConfig) (*S3TransferProvider, error) {
	if settings.Bucket == "" || settings.Region == "" || settings.TransferLifetimeSeconds < 60 || settings.TransferLifetimeSeconds > 3600 {
		return nil, errors.New("SecondBox workspace archive requires bucket, region and transfer lifetime between 60 and 3600 seconds")
	}
	for profile, seconds := range settings.ProfileIdleSeconds {
		if profile == "" || seconds < 1 || seconds > 365*24*3600 {
			return nil, errors.New("SecondBox workspace archive idle policy requires a Profile and a positive duration no greater than one year")
		}
	}
	if path := os.Getenv("AWS_CONFIG_FILE"); path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive AWS shared configuration: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("SecondBox workspace archive AWS shared configuration must be a readable regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive AWS shared configuration: %w", err)
		}
		info, statErr := file.Stat()
		if err := errors.Join(statErr, file.Close()); err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive AWS shared configuration: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("SecondBox workspace archive AWS shared configuration must be a readable regular file")
		}
	}
	configuration, err := config.LoadDefaultConfig(ctx, config.WithRegion(settings.Region))
	if err != nil {
		return nil, fmt.Errorf("SecondBox workspace archive AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(configuration, func(options *s3.Options) {
		options.UsePathStyle = settings.UsePathStyle
		if settings.Endpoint != "" {
			options.BaseEndpoint = aws.String(settings.Endpoint)
		}
	})
	return &S3TransferProvider{client: client, presigner: s3.NewPresignClient(client), bucket: settings.Bucket, lifetime: time.Duration(settings.TransferLifetimeSeconds) * time.Second, credentials: configuration.Credentials, region: settings.Region}, nil
}

func (provider *S3TransferProvider) Transfer(ctx context.Context, key, method string, descriptor *runnerv1.WorkspaceArchiveDescriptor) (_ *runnerv1.WorkspaceArchiveTransfer, transferErr error) {
	if !strings.HasPrefix(key, "workspaces/") || strings.Contains(key, "..") {
		return nil, errors.New("SecondBox workspace archive key is invalid")
	}
	credentials, err := provider.credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("SecondBox workspace archive credentials: %w", err)
	}
	// The SDK cache otherwise retains a process session until its actual expiry,
	// after it has already become too short for an archive grant. Refresh once.
	if credentials.CanExpire && time.Until(credentials.Expires) < 90*time.Second {
		if cache, ok := provider.credentials.(interface{ Invalidate() }); ok {
			cache.Invalidate()
			credentials, err = provider.credentials.Retrieve(ctx)
			if err != nil {
				return nil, fmt.Errorf("SecondBox workspace archive credentials refresh: %w", err)
			}
		}
	}
	lifetime := provider.lifetime
	if credentials.CanExpire {
		lifetime = min(lifetime, time.Until(credentials.Expires)-30*time.Second)
	}
	lifetime = lifetime.Truncate(time.Second)
	if lifetime < time.Minute {
		return nil, errors.New("SecondBox workspace archive credentials expire before the minimum transfer lifetime")
	}
	grant := &runnerv1.WorkspaceArchiveTransfer{ArchiveDescriptor: descriptor, Headers: map[string]string{}, ExpiresAtUnixMs: uint64(time.Now().Add(lifetime).UnixMilli())}
	options := func(options *s3.PresignOptions) { options.Expires = lifetime }
	switch method {
	case http.MethodPut:
		if descriptor == nil || descriptor.ArchiveBytes == 0 {
			return nil, errors.New("SecondBox workspace archive upload requires its bounded capacity")
		}
		partBytes := max(uint64(64<<20), (descriptor.ArchiveBytes+511)/512)
		if partBytes > 5<<30 {
			return nil, errors.New("SecondBox workspace archive exceeds multipart transfer bounds")
		}
		upload, err := provider.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(provider.bucket), Key: aws.String(key)})
		if err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive multipart creation: %w", err)
		}
		defer func() {
			if transferErr == nil {
				return
			}
			cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, abortErr := provider.client.AbortMultipartUpload(cleanupContext, &s3.AbortMultipartUploadInput{Bucket: aws.String(provider.bucket), Key: aws.String(key), UploadId: upload.UploadId})
			if abortErr != nil {
				transferErr = errors.Join(transferErr, fmt.Errorf("SecondBox workspace archive incomplete grant cleanup: %w", abortErr))
			}
		}()
		grant.PartSizeBytes = partBytes
		for part := uint64(1); part <= (descriptor.ArchiveBytes+partBytes-1)/partBytes; part++ {
			request, err := provider.presigner.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(provider.bucket), Key: aws.String(key), UploadId: upload.UploadId, PartNumber: aws.Int32(int32(part))}, options)
			if err != nil {
				return nil, fmt.Errorf("SecondBox workspace archive multipart grant: %w", err)
			}
			grant.PartUrls = append(grant.PartUrls, request.URL)
		}
		complete, err := provider.presignMultipartFinish(ctx, http.MethodPost, grant.PartUrls[0], lifetime)
		if err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive completion grant: %w", err)
		}
		grant.CompleteUrl = complete
		abort, err := provider.presignMultipartFinish(ctx, http.MethodDelete, grant.PartUrls[0], lifetime)
		if err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive abort grant: %w", err)
		}
		grant.AbortUrl = abort
	case http.MethodGet:
		request, err := provider.presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(provider.bucket), Key: aws.String(key)}, options)
		if err != nil {
			return nil, fmt.Errorf("SecondBox workspace archive restore grant: %w", err)
		}
		grant.Url = request.URL
		for key, values := range request.SignedHeader {
			if len(values) > 0 {
				grant.Headers[key] = strings.Join(values, ",")
			}
		}
	default:
		return nil, errors.New("SecondBox workspace archive transfer method is invalid")
	}
	return grant, nil
}

func (provider *S3TransferProvider) presignMultipartFinish(ctx context.Context, method, partURL string, lifetime time.Duration) (string, error) {
	endpoint, err := url.Parse(partURL)
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	for key := range query {
		if strings.HasPrefix(key, "X-Amz-") || key == "partNumber" {
			query.Del(key)
		}
	}
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(lifetime/time.Second), 10))
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	credentials, err := provider.credentials.Retrieve(ctx)
	if err != nil {
		return "", err
	}
	signed, _, err := v4.NewSigner().PresignHTTP(ctx, credentials, request, "UNSIGNED-PAYLOAD", "s3", provider.region, time.Now(), func(options *v4.SignerOptions) { options.DisableURIPathEscaping = true })
	return signed, err
}

func (provider *S3TransferProvider) Delete(ctx context.Context, key string) error {
	if !strings.HasPrefix(key, "workspaces/") || strings.Contains(key, "..") {
		return errors.New("SecondBox workspace archive key is invalid")
	}
	_, err := provider.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(provider.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("SecondBox workspace archive object deletion: %w", err)
	}
	return nil
}
