package errsig

import (
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// Pure unit tests only in this package: every Redis-touching errsig test
// lives in the root package's errsig_handlers_test.go (which owns test DB 9),
// so parallel per-package `go test ./...` runs never flush each other.

func TestNormalizeIdentifiers(t *testing.T) {
	Convey("Given error messages carrying volatile identifiers", t, func() {
		Convey("When the message contains a UUID", func() {
			got := Normalize("task 8f3a2c9d-1b4e-4f6a-9c0d-2e5b7a8f1c3d not found")
			Convey("Then the UUID collapses to ~ID~", func() {
				So(got, ShouldEqual, "task ~ID~ not found")
			})
		})
		Convey("When the message contains a long hex run", func() {
			got := Normalize("commit deadbeef1234 rejected")
			Convey("Then the hex run collapses to ~ID~", func() {
				So(got, ShouldEqual, "commit ~ID~ rejected")
			})
		})
		Convey("When the message contains an 0x-prefixed pointer", func() {
			got := Normalize("segfault at 0xDEADBEEF")
			Convey("Then the pointer collapses to ~ID~", func() {
				So(got, ShouldEqual, "segfault at ~ID~")
			})
		})
		Convey("When the message contains a pure-letter hex word of length 8", func() {
			got := Normalize("marker cafebabe seen")
			Convey("Then it still collapses to ~ID~ (documented trade-off)", func() {
				So(got, ShouldEqual, "marker ~ID~ seen")
			})
		})
		Convey("When a short hex-looking word appears", func() {
			got := Normalize("bad code")
			Convey("Then ordinary words survive", func() {
				So(got, ShouldEqual, "bad code")
			})
		})
		Convey("When hex is embedded inside a longer token", func() {
			got := Normalize("tag task12345678x failed")
			Convey("Then the token is left whole rather than half-replaced", func() {
				So(got, ShouldEqual, "tag task12345678x failed")
			})
		})
		Convey("When two different UUIDs produce two messages", func() {
			a := Normalize("job 0aa87a53-3fc2-4b28-bd94-3f78da1e096e failed")
			b := Normalize("job 92a411be-9032-4d68-b16c-9f0d55b2f0aa failed")
			Convey("Then both normalize to one signature", func() {
				So(a, ShouldEqual, b)
			})
		})
	})
}

func TestNormalizeHosts(t *testing.T) {
	Convey("Given error messages carrying hosts and addresses", t, func() {
		Convey("When the message contains an IPv4 address with a port", func() {
			got := Normalize("connection refused to 10.0.0.7:5432")
			Convey("Then address and port collapse to one ~HOST~", func() {
				So(got, ShouldEqual, "connection refused to ~HOST~")
			})
		})
		Convey("When the message contains a bare IPv4 address", func() {
			got := Normalize("no route to 192.168.1.200")
			Convey("Then it collapses to ~HOST~", func() {
				So(got, ShouldEqual, "no route to ~HOST~")
			})
		})
		Convey("When the message contains a bracketed IPv6 address", func() {
			got := Normalize("dial tcp [2001:db8::1]:443: i/o timeout")
			Convey("Then it collapses to ~HOST~", func() {
				So(got, ShouldEqual, "dial tcp ~HOST~: i/o timeout")
			})
		})
		Convey("When the message contains a URL", func() {
			got := Normalize("gateway timeout POST https://api.stripe.com/v2/charges")
			Convey("Then only the host collapses; scheme, path and the stable /v2 segment survive", func() {
				So(got, ShouldEqual, "gateway timeout POST https://~HOST~/v2/charges")
			})
		})
		Convey("When the message contains a URL with a port", func() {
			got := Normalize("GET http://internal-api.corp.local:8080/health failed")
			Convey("Then host and port collapse together", func() {
				So(got, ShouldEqual, "GET http://~HOST~/health failed")
			})
		})
		Convey("When the message contains a two-label host with a port", func() {
			got := Normalize("connection refused to db-primary.internal:5432")
			Convey("Then it collapses to ~HOST~", func() {
				So(got, ShouldEqual, "connection refused to ~HOST~")
			})
		})
		Convey("When the message contains a two-label infra hostname without a port", func() {
			got := Normalize("cannot resolve redis.local")
			Convey("Then the infra-TLD allowlist catches it", func() {
				So(got, ShouldEqual, "cannot resolve ~HOST~")
			})
		})
		Convey("When the message contains a hex-looking host label", func() {
			got := Normalize("tls: handshake timeout deadbeef.example.com:443")
			Convey("Then the whole host collapses to one ~HOST~, not ~ID~.…", func() {
				So(got, ShouldEqual, "tls: handshake timeout ~HOST~")
			})
		})
		Convey("When the message contains a filename", func() {
			got := Normalize("cannot open video.mp4")
			Convey("Then a two-label non-infra token survives as a literal", func() {
				So(got, ShouldEqual, "cannot open video.mp4")
			})
		})
		Convey("When the message contains a version string", func() {
			got := Normalize("requires billing v2.41.0 or newer")
			Convey("Then it is numbers, not a host (the word-glued v2 stays literal)", func() {
				So(got, ShouldEqual, "requires billing v2.~N~ or newer")
			})
		})
		Convey("When two hosts differ", func() {
			a := Normalize("connect to shard-01.db.internal:6379 refused")
			b := Normalize("connect to shard-07.db.internal:6379 refused")
			Convey("Then both normalize to one signature", func() {
				So(a, ShouldEqual, b)
				So(a, ShouldEqual, "connect to ~HOST~ refused")
			})
		})
	})
}

func TestNormalizeQuotedStrings(t *testing.T) {
	Convey("Given error messages carrying quoted fragments", t, func() {
		Convey("When the message contains a double-quoted string", func() {
			got := Normalize(`unknown flag "verbose-output"`)
			Convey("Then the whole quoted token collapses to ~S~", func() {
				So(got, ShouldEqual, "unknown flag ~S~")
			})
		})
		Convey("When the message contains a backtick-quoted string", func() {
			got := Normalize("column `user_email` does not exist")
			Convey("Then it collapses to ~S~", func() {
				So(got, ShouldEqual, "column ~S~ does not exist")
			})
		})
		Convey("When the message contains a single-quoted string", func() {
			got := Normalize("no such file 'report.csv'")
			Convey("Then it collapses to ~S~", func() {
				So(got, ShouldEqual, "no such file ~S~")
			})
		})
		Convey("When the message contains apostrophes in prose", func() {
			got := Normalize("didn't succeed at the user's request")
			Convey("Then apostrophes never pair into a bogus ~S~", func() {
				So(got, ShouldEqual, "didn't succeed at the user's request")
			})
		})
		Convey("When two messages differ only inside quotes", func() {
			a := Normalize(`invalid value "abc" for field`)
			b := Normalize(`invalid value "xyz" for field`)
			Convey("Then both normalize to one signature", func() {
				So(a, ShouldEqual, b)
			})
		})
	})
}

func TestNormalizeNumbers(t *testing.T) {
	Convey("Given error messages carrying numbers", t, func() {
		Convey("When the message contains a bare integer", func() {
			got := Normalize("ffmpeg exited 137")
			Convey("Then it collapses to ~N~", func() {
				So(got, ShouldEqual, "ffmpeg exited ~N~")
			})
		})
		Convey("When the message contains a duration", func() {
			got := Normalize("context deadline exceeded after 30s")
			Convey("Then the number collapses but the unit survives", func() {
				So(got, ShouldEqual, "context deadline exceeded after ~N~s")
			})
		})
		Convey("When two durations differ", func() {
			a := Normalize("context deadline exceeded after 30s")
			b := Normalize("context deadline exceeded after 31s")
			Convey("Then both normalize to one signature", func() {
				So(a, ShouldEqual, b)
			})
		})
		Convey("When the message contains a decimal", func() {
			got := Normalize("rate 8.25 over budget")
			Convey("Then the whole decimal collapses to one ~N~", func() {
				So(got, ShouldEqual, "rate ~N~ over budget")
			})
		})
		Convey("When the message contains a byte size", func() {
			got := Normalize("payload exceeds 10MiB limit")
			Convey("Then the number collapses and the unit survives", func() {
				So(got, ShouldEqual, "payload exceeds ~N~MiB limit")
			})
		})
		Convey("When the message contains an HTTP status", func() {
			a := Normalize("upstream returned status 502")
			b := Normalize("upstream returned status 503")
			Convey("Then different statuses share one signature (documented trade-off)", func() {
				So(a, ShouldEqual, b)
				So(a, ShouldEqual, "upstream returned status ~N~")
			})
		})
	})
}

func TestNormalizeShapeAndBounds(t *testing.T) {
	Convey("Given messy real-world inputs", t, func() {
		Convey("When the message has irregular whitespace", func() {
			got := Normalize("  too\tmany \n  spaces  ")
			Convey("Then whitespace collapses to single spaces, trimmed", func() {
				So(got, ShouldEqual, "too many spaces")
			})
		})
		Convey("When the message is empty", func() {
			Convey("Then the template is empty", func() {
				So(Normalize(""), ShouldEqual, "")
			})
		})
		Convey("When the message is enormous", func() {
			got := Normalize("prefix " + strings.Repeat("x", 5000))
			Convey("Then the template is bounded", func() {
				So(len(got), ShouldBeLessThanOrEqualTo, maxTemplateLen)
				So(strings.HasPrefix(got, "prefix "), ShouldBeTrue)
			})
		})
		Convey("When the mockup's flagship error arrives", func() {
			got := Normalize("gateway timeout POST https://api-3.stripe.com/v2/charges")
			Convey("Then it matches the mockup's template exactly", func() {
				So(got, ShouldEqual, "gateway timeout POST https://~HOST~/v2/charges")
			})
		})
		Convey("When normalization runs twice", func() {
			once := Normalize("connect to 10.0.0.7:5432 failed after 30s")
			twice := Normalize(once)
			Convey("Then it is idempotent for its own placeholders", func() {
				So(twice, ShouldEqual, once)
			})
		})
	})
}

func TestSigHash(t *testing.T) {
	Convey("Given signature templates", t, func() {
		Convey("When hashing the same template twice", func() {
			Convey("Then the hash is stable", func() {
				So(SigHash("a b c"), ShouldEqual, SigHash("a b c"))
			})
		})
		Convey("When hashing different templates", func() {
			Convey("Then the hashes differ", func() {
				So(SigHash("a"), ShouldNotEqual, SigHash("b"))
			})
		})
		Convey("When inspecting the hash shape", func() {
			h := SigHash("connection refused to ~HOST~")
			Convey("Then it is 32 lowercase hex chars", func() {
				So(len(h), ShouldEqual, 32)
				So(h, ShouldEqual, strings.ToLower(h))
			})
		})
	})
}
