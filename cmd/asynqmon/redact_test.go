package main

import (
	"errors"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// A redis URI carries its password in the userinfo. asynq and go-redis echo
// the URI they could not parse, and that error reaches log.Fatal, so the
// message must not carry the secret.
func TestRedactURIError(t *testing.T) {
	Convey("Given a parse error that echoes the whole redis URI", t, func() {
		raw := "redis://user:s3cret@redis.example:6379/0"
		err := errors.New(`asynq: could not parse "redis://user:s3cret@redis.example:6379/0"`)

		Convey("When the error is redacted", func() {
			got := redactURIError(err, raw)

			Convey("Then the password is gone and the host survives", func() {
				So(got, ShouldNotBeNil)
				So(strings.Contains(got.Error(), "s3cret"), ShouldBeFalse)
				So(got.Error(), ShouldContainSubstring, "***")
				So(got.Error(), ShouldContainSubstring, "redis.example:6379")
			})
		})
	})

	Convey("Given a URI with no password", t, func() {
		err := errors.New("asynq: unsupported uri scheme")

		Convey("When the error is redacted", func() {
			got := redactURIError(err, "redis://redis.example:6379/0")

			Convey("Then the message is unchanged", func() {
				So(got.Error(), ShouldEqual, "asynq: unsupported uri scheme")
			})
		})
	})

	Convey("Given no error", t, func() {
		Convey("Then redaction returns nil", func() {
			So(redactURIError(nil, "redis://user:s3cret@h:6379"), ShouldBeNil)
		})
	})
}
