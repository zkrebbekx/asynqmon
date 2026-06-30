package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// sha256 of an empty body — the payload hash for the (body-less) presign.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

const (
	elastiCacheService = "elasticache"
	iamTokenExpiry     = 900 * time.Second // ElastiCache IAM tokens are valid 15m
)

// awsIAMProvider mints AWS ElastiCache IAM auth tokens (a SigV4-presigned
// "connect" request) to use as the Redis password.
type awsIAMProvider struct {
	cacheName string
	user      string
	region    string
	creds     aws.CredentialsProvider
	signer    *v4.Signer
}

func newAWSIAMProvider(ctx context.Context, cfg *Config) (*awsIAMProvider, error) {
	if cfg.RedisIAMUser == "" {
		return nil, fmt.Errorf("--redis-user is required for AWS IAM auth (the ElastiCache RBAC user)")
	}
	if cfg.RedisIAMCacheName == "" {
		return nil, fmt.Errorf("--redis-iam-cache-name is required for AWS IAM auth (the ElastiCache cache/replication-group name)")
	}
	if cfg.AWSRegion == "" {
		return nil, fmt.Errorf("--aws-region is required for AWS IAM auth")
	}
	awscfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &awsIAMProvider{
		cacheName: cfg.RedisIAMCacheName,
		user:      cfg.RedisIAMUser,
		region:    cfg.AWSRegion,
		creds:     awscfg.Credentials,
		signer:    v4.NewSigner(),
	}, nil
}

func (p *awsIAMProvider) Name() string { return "aws" }

func (p *awsIAMProvider) Credentials(ctx context.Context) (string, string, error) {
	token, err := p.buildToken(ctx, time.Now())
	if err != nil {
		return "", "", err
	}
	return p.user, token, nil
}

// buildToken returns the ElastiCache IAM auth token (the SigV4-presigned URL with
// the scheme stripped). Split out for testing.
func (p *awsIAMProvider) buildToken(ctx context.Context, now time.Time) (string, error) {
	q := url.Values{}
	q.Set("Action", "connect")
	q.Set("User", p.user)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(iamTokenExpiry.Seconds())))

	reqURL := fmt.Sprintf("https://%s/?%s", p.cacheName, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}

	creds, err := p.creds.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("retrieve AWS credentials: %w", err)
	}

	signedURL, _, err := p.signer.PresignHTTP(ctx, creds, req, emptyPayloadHash, elastiCacheService, p.region, now)
	if err != nil {
		return "", fmt.Errorf("presign ElastiCache IAM request: %w", err)
	}
	// The auth token is the signed request without its scheme.
	return strings.TrimPrefix(signedURL, "https://"), nil
}
