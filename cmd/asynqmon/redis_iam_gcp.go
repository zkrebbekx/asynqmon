package main

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// gcpScope is the OAuth2 scope used for Memorystore IAM authentication.
const gcpScope = "https://www.googleapis.com/auth/cloud-platform"

// gcpIAMProvider authenticates to GCP Memorystore for Redis using a Google
// OAuth2 access token (from Application Default Credentials) as the password.
// The token source caches and refreshes the token automatically.
type gcpIAMProvider struct {
	user        string
	tokenSource oauth2.TokenSource
}

func newGCPIAMProvider(ctx context.Context, cfg *Config) (*gcpIAMProvider, error) {
	creds, err := google.FindDefaultCredentials(ctx, gcpScope)
	if err != nil {
		return nil, fmt.Errorf("find GCP default credentials: %w", err)
	}
	user := cfg.RedisIAMUser
	if user == "" {
		// Memorystore IAM AUTH uses the IAM principal; "default" is the
		// conventional username when none is specified.
		user = "default"
	}
	return &gcpIAMProvider{
		user:        user,
		tokenSource: creds.TokenSource,
	}, nil
}

func (p *gcpIAMProvider) Name() string { return "gcp" }

func (p *gcpIAMProvider) Credentials(ctx context.Context) (string, string, error) {
	tok, err := p.tokenSource.Token()
	if err != nil {
		return "", "", fmt.Errorf("get GCP access token: %w", err)
	}
	return p.user, tok.AccessToken, nil
}
