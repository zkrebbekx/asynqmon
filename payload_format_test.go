package asynqmon

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func gzipped(s string) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(s))
	w.Close()
	return buf.Bytes()
}

func TestSmartPayloadFormatter(t *testing.T) {
	Convey("Given printable payload bytes", t, func() {
		Convey("They are returned unchanged", func() {
			So(formatBytes([]byte(`{"asset_id":"26835"}`)), ShouldEqual, `{"asset_id":"26835"}`)
		})
		Convey("An empty payload formats to an empty string", func() {
			So(formatBytes(nil), ShouldEqual, "")
		})
	})

	Convey("Given a gzip-compressed JSON payload", t, func() {
		out := formatBytes(gzipped(`{"asset_id":"26835"}`))

		Convey("It is decompressed and shown as text", func() {
			So(out, ShouldEqual, `{"asset_id":"26835"}`)
		})
	})

	Convey("Given a msgpack-encoded map", t, func() {
		// fixmap(1) { fixstr("asset_id"): uint16(26835) }
		data := []byte{0x81, 0xa8, 'a', 's', 's', 'e', 't', '_', 'i', 'd', 0xcd, 0x68, 0xd3}
		out := formatBytes(data)

		Convey("It is decoded and rendered as JSON", func() {
			So(out, ShouldContainSubstring, `"asset_id"`)
			So(out, ShouldContainSubstring, "26835")
		})
		Convey("It is not reported as non-printable", func() {
			So(out, ShouldNotContainSubstring, "non-printable")
		})
	})

	Convey("Given a msgpack payload with nested containers", t, func() {
		// fixmap(1) { fixstr("ids"): fixarray(2)[ 1, 2 ] }
		data := []byte{0x81, 0xa3, 'i', 'd', 's', 0x92, 0x01, 0x02}

		Convey("The nesting is preserved in the JSON output", func() {
			out := formatBytes(data)
			So(out, ShouldContainSubstring, `"ids"`)
			So(out, ShouldContainSubstring, "1")
			So(out, ShouldContainSubstring, "2")
		})
	})

	Convey("Given bytes that decode as msgpack but leave trailing data", t, func() {
		data := []byte{0x81, 0xa1, 'a', 0x01, 0xff, 0xfe}

		Convey("The partial decode is rejected in favour of a hex dump", func() {
			out := formatBytes(data)
			So(out, ShouldContainSubstring, "binary payload")
		})
	})

	Convey("Given undecodable binary bytes", t, func() {
		data := []byte{0x00, 0x01, 0x02, 'h', 'i', 0xff, 0xfe, 0xfd}
		out := formatBytes(data)

		Convey("The byte count is reported", func() {
			So(out, ShouldContainSubstring, "binary payload (8 bytes)")
		})
		Convey("Printable runs are still readable in the text preview", func() {
			So(out, ShouldContainSubstring, "text: ...hi...")
		})
		Convey("The raw bytes are shown as hex", func() {
			So(out, ShouldContainSubstring, "0001 02")
		})
		Convey("The contents are never hidden behind a placeholder", func() {
			So(out, ShouldNotContainSubstring, "non-printable")
		})
	})

	Convey("Given a binary payload larger than the dump limit", t, func() {
		data := bytes.Repeat([]byte{0xff}, maxHexDumpBytes+50)
		out := formatBytes(data)

		Convey("The full size is reported and the remainder noted", func() {
			So(out, ShouldContainSubstring, "binary payload (562 bytes)")
			So(out, ShouldContainSubstring, "... 50 more bytes")
		})
	})

	Convey("Given a decompression bomb", t, func() {
		data := gzipped(strings.Repeat("a", maxDecompressedSize+1))

		Convey("It is not inflated, and falls back to a hex dump", func() {
			out := formatBytes(data)
			So(out, ShouldContainSubstring, "binary payload")
		})
	})
}

func TestMsgpackToJSON(t *testing.T) {
	Convey("Given msgpack scalars at the top level", t, func() {
		Convey("They are rejected, since only containers are trusted as a decode", func() {
			_, ok := msgpackToJSON([]byte{0x01})
			So(ok, ShouldBeFalse)
		})
	})

	Convey("Given a msgpack map with a non-string key", t, func() {
		// fixmap(1) { 1: 2 }
		out, ok := msgpackToJSON([]byte{0x81, 0x01, 0x02})

		Convey("The key is stringified so the value still renders", func() {
			So(ok, ShouldBeTrue)
			So(out, ShouldContainSubstring, `"1"`)
		})
	})

	Convey("Given a msgpack string containing invalid UTF-8", t, func() {
		// fixmap(1) { fixstr(1)=0xff : 1 }
		_, ok := msgpackToJSON([]byte{0x81, 0xa1, 0xff, 0x01})

		Convey("The decode is rejected rather than emitting mojibake", func() {
			So(ok, ShouldBeFalse)
		})
	})

	Convey("Given a truncated msgpack container", t, func() {
		Convey("The decode fails instead of panicking", func() {
			_, ok := msgpackToJSON([]byte{0x81, 0xa3, 'i', 'd'})
			So(ok, ShouldBeFalse)
		})
	})

	Convey("Given a container header claiming more elements than the input holds", t, func() {
		Convey("The decode fails instead of allocating on the claimed size", func() {
			// map32 claiming ~4 billion entries with no body.
			_, ok := msgpackToJSON([]byte{0xdf, 0xff, 0xff, 0xff, 0xff})
			So(ok, ShouldBeFalse)
		})
	})
}
