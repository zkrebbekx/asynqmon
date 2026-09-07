package main

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// Identity hardening flags (#32), the SSE cap (#35) and the hygiene run-now
// gate (#53): flag and env parsing plus the auth-header trust validation.
// Parsing runs imperatively before the Convey tree (repo discipline).
func TestIdentityHardeningFlags(t *testing.T) {
	defCfg, _, defErr := parseFlags("asynqmon", []string{})
	flagCfg, _, flagErr := parseFlags("asynqmon", []string{
		"--trust-basic-auth-user", "--allow-untrusted-auth-header",
		"--max-sse-connections", "12", "--hygiene-run-in-read-only",
	})
	t.Setenv("TRUST_BASIC_AUTH_USER", "true")
	t.Setenv("ALLOW_UNTRUSTED_AUTH_HEADER", "true")
	t.Setenv("MAX_SSE_CONNECTIONS", "7")
	t.Setenv("HYGIENE_RUN_IN_READ_ONLY", "true")
	envCfg, _, envErr := parseFlags("asynqmon", []string{})

	Convey("Given the asynqmon command-line flag set", t, func() {
		Convey("When no flag or env is given", func() {
			Convey("Then every hardening option is off and the SSE cap is 256", func() {
				So(defErr, ShouldBeNil)
				So(defCfg.TrustBasicAuthUser, ShouldBeFalse)
				So(defCfg.AllowUntrustedAuthHeader, ShouldBeFalse)
				So(defCfg.MaxSSEConnections, ShouldEqual, 256)
				So(defCfg.HygieneRunInReadOnly, ShouldBeFalse)
			})
		})
		Convey("When the flags are given", func() {
			Convey("Then the config carries them", func() {
				So(flagErr, ShouldBeNil)
				So(flagCfg.TrustBasicAuthUser, ShouldBeTrue)
				So(flagCfg.AllowUntrustedAuthHeader, ShouldBeTrue)
				So(flagCfg.MaxSSEConnections, ShouldEqual, 12)
				So(flagCfg.HygieneRunInReadOnly, ShouldBeTrue)
			})
		})
		Convey("When the env vars are set", func() {
			Convey("Then the config carries them", func() {
				So(envErr, ShouldBeNil)
				So(envCfg.TrustBasicAuthUser, ShouldBeTrue)
				So(envCfg.AllowUntrustedAuthHeader, ShouldBeTrue)
				So(envCfg.MaxSSEConnections, ShouldEqual, 7)
				So(envCfg.HygieneRunInReadOnly, ShouldBeTrue)
			})
		})
	})
}

func TestValidateAuthHeaderTrust(t *testing.T) {
	Convey("Given the startup validation of --auth-header trust", t, func() {
		Convey("When --auth-header is set and --trusted-proxies is empty", func() {
			msg := validateAuthHeaderTrust(&Config{AuthHeader: "X-User"})
			Convey("Then startup is refused with a message naming both flags", func() {
				So(msg, ShouldContainSubstring, "--auth-header needs --trusted-proxies")
				So(msg, ShouldContainSubstring, "--allow-untrusted-auth-header")
			})
		})
		Convey("When --require-identity is also set", func() {
			msg := validateAuthHeaderTrust(&Config{AuthHeader: "X-User", RequireIdentity: true})
			Convey("Then the message says the requirement is forgeable too", func() {
				So(msg, ShouldContainSubstring, "--require-identity")
			})
		})
		Convey("When --allow-untrusted-auth-header acknowledges the risk", func() {
			msg := validateAuthHeaderTrust(&Config{AuthHeader: "X-User", AllowUntrustedAuthHeader: true})
			Convey("Then startup proceeds", func() {
				So(msg, ShouldBeEmpty)
			})
		})
		Convey("When --trusted-proxies is set", func() {
			msg := validateAuthHeaderTrust(&Config{AuthHeader: "X-User", TrustedProxies: "10.0.0.0/8"})
			Convey("Then startup proceeds", func() {
				So(msg, ShouldBeEmpty)
			})
		})
		Convey("When --auth-header is not set", func() {
			msg := validateAuthHeaderTrust(&Config{})
			Convey("Then there is nothing to validate", func() {
				So(msg, ShouldBeEmpty)
			})
		})
	})
}
