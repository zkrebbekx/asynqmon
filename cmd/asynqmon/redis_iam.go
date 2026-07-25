package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// credentialsProvider returns short-lived credentials for a new Redis
// connection. Implementations are cloud-specific (AWS ElastiCache IAM, GCP
// Memorystore IAM, ...) but the rest of asynqmon stays provider-agnostic.
type credentialsProvider interface {
	// Credentials returns the username and password (auth token) to use for a
	// new connection. It is called by go-redis per connection, so it must return
	// a currently-valid token (providers refresh/cache internally).
	Credentials(ctx context.Context) (username, password string, err error)
	// Name identifies the provider for logging/errors.
	Name() string
}

// newCredentialsProvider builds the provider selected by --redis-iam-provider.
func newCredentialsProvider(ctx context.Context, cfg *Config) (credentialsProvider, error) {
	switch strings.ToLower(cfg.RedisIAMProvider) {
	case "aws":
		return newAWSIAMProvider(ctx, cfg)
	case "gcp":
		return newGCPIAMProvider(ctx, cfg)
	case "":
		return nil, fmt.Errorf("--redis-iam-provider is required when --redis-iam-auth is set (want: aws|gcp)")
	default:
		return nil, fmt.Errorf("unknown --redis-iam-provider %q (want: aws|gcp)", cfg.RedisIAMProvider)
	}
}

// iamRedisConnOpt is an asynq.RedisConnOpt that builds a go-redis client whose
// credentials are minted per-connection by a credentialsProvider. Cluster mode.
type iamRedisConnOpt struct {
	addrs     []string
	provider  credentialsProvider
	tlsConfig *tls.Config
}

// MakeRedisClient implements asynq.RedisConnOpt.
//
// The IAM token is short-lived, so we authenticate per connection in OnConnect
// (issuing AUTH with a freshly-minted token) rather than via a static password.
// OnConnect is used instead of CredentialsProviderContext for compatibility
// with the go-redis version pinned by asynq.
func (o iamRedisConnOpt) MakeRedisClient() interface{} {
	return redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:     o.addrs,
		TLSConfig: o.tlsConfig,
		OnConnect: func(ctx context.Context, cn *redis.Conn) error {
			user, pass, err := o.provider.Credentials(ctx)
			if err != nil {
				return err
			}
			if user != "" {
				return cn.AuthACL(ctx, user, pass).Err()
			}
			return cn.Auth(ctx, pass).Err()
		},
	})
}

// makeIAMRedisConnOpt assembles the IAM-authenticated connection option.
func makeIAMRedisConnOpt(ctx context.Context, cfg *Config) (asynq.RedisConnOpt, error) {
	provider, err := newCredentialsProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}

	var addrs []string
	if len(cfg.RedisClusterNodes) > 0 {
		addrs = strings.Split(cfg.RedisClusterNodes, ",")
	} else if cfg.RedisAddr != "" {
		addrs = []string{cfg.RedisAddr}
	} else {
		return nil, fmt.Errorf("redis address is required for IAM auth (set --redis-addr or --redis-cluster-nodes)")
	}

	// IAM auth requires in-transit encryption; ensure a TLS config is present.
	tlsConfig := makeTLSConfig(cfg)
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return iamRedisConnOpt{
		addrs:     addrs,
		provider:  provider,
		tlsConfig: tlsConfig,
	}, nil
}
