package workspacearchive

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestWorkspaceArchiveGrantDoesNotOutliveRoleCredentials(t *testing.T) {
	credentials := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "test-access-key", SecretAccessKey: "test-secret", SessionToken: "test-session", CanExpire: true, Expires: time.Now().Add(5 * time.Minute)}, nil
	})
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials})
	provider := &S3TransferProvider{client: client, presigner: s3.NewPresignClient(client), bucket: "archive-test", region: "us-east-1", lifetime: time.Hour, credentials: credentials}
	grant, err := provider.Transfer(t.Context(), "workspaces/test", http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(grant.Url)
	if err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.Atoi(parsed.Query().Get("X-Amz-Expires"))
	if err != nil || seconds < 260 || seconds > 270 {
		t.Fatalf("role-bound grant expires=%d err=%v", seconds, err)
	}
	if grant.ExpiresAtUnixMs > uint64(time.Now().Add(271*time.Second).UnixMilli()) {
		t.Fatal("grant advertises a deadline past credential expiry margin")
	}
}

func TestWorkspaceArchiveRequiresReadableExplicitAWSConfiguration(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing-config"), t.TempDir()} {
		t.Setenv("AWS_CONFIG_FILE", path)
		_, err := NewS3TransferProvider(t.Context(), ObjectStoreConfig{Bucket: "test", Region: "us-east-1", TransferLifetimeSeconds: 600})
		if err == nil {
			t.Fatalf("invalid explicit AWS configuration accepted: %s", path)
		}
	}
}

func TestWorkspaceArchiveRefreshesCachedSessionBeforeGrantWindowCloses(t *testing.T) {
	for _, renewalLifetime := range []time.Duration{5 * time.Minute, time.Minute} {
		calls := 0
		credentials := aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			calls++
			lifetime := time.Minute
			if calls > 1 {
				lifetime = renewalLifetime
			}
			return aws.Credentials{AccessKeyID: "test-key", SecretAccessKey: "test-secret", SessionToken: "test-session", CanExpire: true, Expires: time.Now().Add(lifetime)}, nil
		}))
		client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials})
		provider := &S3TransferProvider{client: client, presigner: s3.NewPresignClient(client), bucket: "archive-test", region: "us-east-1", lifetime: time.Hour, credentials: credentials}
		grant, err := provider.Transfer(t.Context(), "workspaces/test", http.MethodGet, nil)
		if calls != 2 {
			t.Fatalf("credential retrieval count = %d, want one refresh", calls)
		}
		if renewalLifetime == time.Minute {
			if err == nil {
				t.Fatal("still-expiring refreshed credentials issued an archive grant")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(grant.Url)
		if err != nil {
			t.Fatal(err)
		}
		seconds, err := strconv.Atoi(parsed.Query().Get("X-Amz-Expires"))
		if err != nil || seconds < 260 || seconds > 270 {
			t.Fatalf("refreshed role grant expires=%d err=%v", seconds, err)
		}
	}
}

func TestWorkspaceArchiveRefusesCredentialsTooCloseToExpiry(t *testing.T) {
	provider := &S3TransferProvider{lifetime: time.Hour, credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{CanExpire: true, Expires: time.Now().Add(time.Minute)}, nil
	})}
	if _, err := provider.Transfer(t.Context(), "workspaces/test", http.MethodGet, nil); err == nil {
		t.Fatal("near-expired credentials issued an archive grant")
	}
}
