package main

import (
	"context"
	"strings"
	"testing"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	. "github.com/smartystreets/goconvey/convey"
	"golang.org/x/oauth2"
)

func TestNewCredentialsProvider(t *testing.T) {
	Convey("Given the IAM provider factory", t, func() {
		ctx := context.Background()

		Convey("An empty provider is rejected", func() {
			_, err := newCredentialsProvider(ctx, &Config{RedisIAMProvider: ""})
			So(err, ShouldNotBeNil)
		})
		Convey("An unknown provider is rejected", func() {
			_, err := newCredentialsProvider(ctx, &Config{RedisIAMProvider: "azure"})
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "aws|gcp")
		})
	})
}

func TestAWSBuildToken(t *testing.T) {
	Convey("Given an AWS IAM provider with static credentials", t, func() {
		p := &awsIAMProvider{
			cacheName: "my-cache",
			user:      "iam-user",
			region:    "us-east-1",
			creds:     credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
			signer:    v4.NewSigner(),
		}

		Convey("buildToken produces a SigV4-presigned ElastiCache connect token", func() {
			tok, err := p.buildToken(context.Background(), time.Unix(1700000000, 0).UTC())
			So(err, ShouldBeNil)
			// No scheme — the token is host+query.
			So(tok, ShouldNotStartWith, "https://")
			So(tok, ShouldStartWith, "my-cache/")
			So(tok, ShouldContainSubstring, "Action=connect")
			So(tok, ShouldContainSubstring, "User=iam-user")
			So(tok, ShouldContainSubstring, "X-Amz-Algorithm=AWS4-HMAC-SHA256")
			So(tok, ShouldContainSubstring, "X-Amz-Credential=")
			So(tok, ShouldContainSubstring, "X-Amz-Signature=")
		})

		Convey("Credentials returns the configured user with the token", func() {
			user, pass, err := p.Credentials(context.Background())
			So(err, ShouldBeNil)
			So(user, ShouldEqual, "iam-user")
			So(pass, ShouldNotBeEmpty)
		})
	})
}

func TestGCPCredentials(t *testing.T) {
	Convey("Given a GCP IAM provider with an injected token source", t, func() {
		p := &gcpIAMProvider{
			user:        "default",
			tokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.test-token"}),
		}

		Convey("Credentials returns the user and the access token", func() {
			user, pass, err := p.Credentials(context.Background())
			So(err, ShouldBeNil)
			So(user, ShouldEqual, "default")
			So(pass, ShouldEqual, "ya29.test-token")
		})
	})
}

func TestMakeIAMRedisConnOptValidation(t *testing.T) {
	Convey("makeIAMRedisConnOpt validates required fields", t, func() {
		ctx := context.Background()
		Convey("Unknown provider errors before building a client", func() {
			_, err := makeIAMRedisConnOpt(ctx, &Config{RedisIAMProvider: "nope", RedisAddr: "x:6379"})
			So(err, ShouldNotBeNil)
		})
	})
}

// ensure the IAM conn opt satisfies the asynq interface used by asynqmon.
func TestIAMConnOptType(t *testing.T) {
	Convey("iamRedisConnOpt implements asynq.RedisConnOpt", t, func() {
		var _ interface{ MakeRedisClient() interface{} } = iamRedisConnOpt{}
		So(strings.Contains("ok", "ok"), ShouldBeTrue)
	})
}
