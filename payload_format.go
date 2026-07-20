package asynqmon

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

// maxDecompressedSize bounds how many bytes we are willing to inflate from a
// compressed payload. Payloads are attacker-influenced data from the queue, so
// a decompression bomb must not be able to exhaust the server's memory.
const maxDecompressedSize = 1 << 20 // 1MB

// maxHexDumpBytes bounds how much of an undecodable payload we render as a hex
// dump. The dump is several times the size of the input, and the UI truncates
// the formatted string anyway (--max-payload-length).
const maxHexDumpBytes = 512

// SmartPayloadFormatter renders payload bytes for display, falling back
// through progressively more generic strategies so that the bytes are always
// visible in some form:
//
//  1. printable text (JSON is pretty-printed by the UI) is returned as is
//  2. gzip/zlib-compressed data is decompressed and re-formatted
//  3. msgpack-encoded maps and arrays are decoded and rendered as JSON
//  4. anything else is rendered as a hex + ASCII dump
//
// Unlike DefaultPayloadFormatter it never hides the contents behind a
// "non-printable bytes" placeholder.
var SmartPayloadFormatter = PayloadFormatterFunc(func(_ string, payload []byte) string {
	return formatBytes(payload)
})

// SmartResultFormatter is SmartPayloadFormatter for task results.
var SmartResultFormatter = ResultFormatterFunc(func(_ string, result []byte) string {
	return formatBytes(result)
})

func formatBytes(data []byte) string {
	return formatBytesDepth(data, 0)
}

// formatBytesDepth formats data, recursing at most once into decompressed
// content so that a nested-compression payload cannot loop.
func formatBytesDepth(data []byte, depth int) string {
	if len(data) == 0 {
		return ""
	}
	if isPrintable(data) {
		return string(data)
	}
	if depth == 0 {
		if inflated, ok := decompress(data); ok {
			return formatBytesDepth(inflated, depth+1)
		}
	}
	if s, ok := msgpackToJSON(data); ok {
		return s
	}
	return hexDump(data)
}

// decompress inflates gzip- or zlib-compressed data. It reports false when the
// data is not compressed or cannot be fully inflated within the size limit.
func decompress(data []byte) ([]byte, bool) {
	var r io.ReadCloser
	var err error
	switch {
	case len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b: // gzip magic
		r, err = gzip.NewReader(bytes.NewReader(data))
	case len(data) > 2 && data[0] == 0x78: // zlib CMF for deflate w/ 32K window
		r, err = zlib.NewReader(bytes.NewReader(data))
	default:
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	defer r.Close()

	// Read one byte past the limit so an oversized stream is detected rather
	// than silently truncated into misleading output.
	out, err := io.ReadAll(io.LimitReader(r, maxDecompressedSize+1))
	if err != nil || len(out) > maxDecompressedSize || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// hexDump renders data as an ASCII preview followed by hex bytes, prefixed
// with the payload size so a truncated dump is not mistaken for the whole
// payload.
//
// The text line comes first deliberately: the UI truncates the formatted
// string to --max-payload-length (200 chars by default), and for the common
// case of a binary envelope around text (msgpack, protobuf, gob) the readable
// field names are what identify the task at a glance.
func hexDump(data []byte) string {
	shown := data
	if len(shown) > maxHexDumpBytes {
		shown = shown[:maxHexDumpBytes]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "binary payload (%d bytes)\n", len(data))
	fmt.Fprintf(&b, "text: %s\n", asciiPreview(shown))
	fmt.Fprintf(&b, "hex:  %s", hexGrouped(shown))
	if len(shown) < len(data) {
		fmt.Fprintf(&b, "\n... %d more bytes", len(data)-len(shown))
	}
	return b.String()
}

// asciiPreview renders printable ASCII as is and everything else as ".", the
// same convention as hexdump/xxd.
func asciiPreview(data []byte) string {
	out := make([]byte, len(data))
	for i, c := range data {
		if c >= 0x20 && c <= 0x7e {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}

// hexGrouped renders data as hex, space separated every 2 bytes, so long runs
// stay scannable without the line-per-16-bytes overhead of hex.Dump.
func hexGrouped(data []byte) string {
	var b strings.Builder
	b.Grow(len(data)*2 + len(data)/2)
	for i := 0; i < len(data); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}
		end := i + 2
		if end > len(data) {
			end = len(data)
		}
		b.WriteString(hex.EncodeToString(data[i:end]))
	}
	return b.String()
}

// msgpackToJSON decodes a msgpack-encoded map or array and re-renders it as
// indented JSON. It reports false unless the whole input decodes cleanly into
// a container, which keeps arbitrary binary from being coerced into a
// plausible-looking but wrong value.
func msgpackToJSON(data []byte) (string, bool) {
	d := &msgpackDecoder{buf: data}
	v, err := d.decodeValue(0)
	if err != nil || d.pos != len(data) {
		return "", false
	}
	switch v.(type) {
	case map[string]interface{}, []interface{}:
	default:
		return "", false
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", false
	}
	return string(out), true
}

// msgpackDecoder is a minimal msgpack reader. It covers the value types a task
// payload realistically uses; ext types and unknown prefixes are rejected so
// that a failed decode falls back to the hex dump.
type msgpackDecoder struct {
	buf []byte
	pos int
}

const msgpackMaxDepth = 32

var errMsgpack = errors.New("not msgpack")

func (d *msgpackDecoder) next(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.buf) {
		return nil, errMsgpack
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *msgpackDecoder) byte() (byte, error) {
	b, err := d.next(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// uint reads an n-byte big-endian unsigned integer.
func (d *msgpackDecoder) uint(n int) (uint64, error) {
	b, err := d.next(n)
	if err != nil {
		return 0, err
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

func (d *msgpackDecoder) decodeValue(depth int) (interface{}, error) {
	if depth > msgpackMaxDepth {
		return nil, errMsgpack
	}
	c, err := d.byte()
	if err != nil {
		return nil, err
	}
	switch {
	case c <= 0x7f: // positive fixint
		return int64(c), nil
	case c >= 0xe0: // negative fixint
		return int64(int8(c)), nil
	case c >= 0x80 && c <= 0x8f: // fixmap
		return d.decodeMap(int(c&0x0f), depth)
	case c >= 0x90 && c <= 0x9f: // fixarray
		return d.decodeArray(int(c&0x0f), depth)
	case c >= 0xa0 && c <= 0xbf: // fixstr
		return d.decodeString(int(c & 0x1f))
	}
	switch c {
	case 0xc0:
		return nil, nil
	case 0xc2:
		return false, nil
	case 0xc3:
		return true, nil
	case 0xc4, 0xc5, 0xc6: // bin 8/16/32
		n, err := d.uint(1 << (c - 0xc4))
		if err != nil {
			return nil, err
		}
		return d.decodeBinary(n)
	case 0xca: // float32
		v, err := d.uint(4)
		if err != nil {
			return nil, err
		}
		return float64(math.Float32frombits(uint32(v))), nil
	case 0xcb: // float64
		v, err := d.uint(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(v), nil
	case 0xcc, 0xcd, 0xce, 0xcf: // uint 8/16/32/64
		v, err := d.uint(1 << (c - 0xcc))
		if err != nil {
			return nil, err
		}
		return v, nil
	case 0xd0, 0xd1, 0xd2, 0xd3: // int 8/16/32/64
		n := 1 << (c - 0xd0)
		v, err := d.uint(n)
		if err != nil {
			return nil, err
		}
		return signed(v, n), nil
	case 0xd9, 0xda, 0xdb: // str 8/16/32
		n, err := d.uint(1 << (c - 0xd9))
		if err != nil {
			return nil, err
		}
		return d.decodeStringN(n)
	case 0xdc, 0xdd: // array 16/32
		n, err := d.uint(2 << (c - 0xdc))
		if err != nil {
			return nil, err
		}
		if n > uint64(len(d.buf)) {
			return nil, errMsgpack
		}
		return d.decodeArray(int(n), depth)
	case 0xde, 0xdf: // map 16/32
		n, err := d.uint(2 << (c - 0xde))
		if err != nil {
			return nil, err
		}
		if n > uint64(len(d.buf)) {
			return nil, errMsgpack
		}
		return d.decodeMap(int(n), depth)
	}
	// fixext/ext/timestamp and reserved prefixes: not supported.
	return nil, errMsgpack
}

// signed reinterprets the low n bytes of v as a two's complement integer.
func signed(v uint64, n int) int64 {
	switch n {
	case 1:
		return int64(int8(v))
	case 2:
		return int64(int16(v))
	case 4:
		return int64(int32(v))
	default:
		return int64(v)
	}
}

func (d *msgpackDecoder) decodeString(n int) (string, error) {
	return d.decodeStringN(uint64(n))
}

func (d *msgpackDecoder) decodeStringN(n uint64) (string, error) {
	if n > uint64(len(d.buf)) {
		return "", errMsgpack
	}
	b, err := d.next(int(n))
	if err != nil {
		return "", err
	}
	// A msgpack str must be valid UTF-8. Rejecting invalid strings keeps
	// random binary from decoding into mojibake that looks like real data.
	if !utf8.Valid(b) {
		return "", errMsgpack
	}
	return string(b), nil
}

// decodeBinary renders a msgpack bin value. Nested binary is hex encoded so it
// survives JSON marshalling in a readable form.
func (d *msgpackDecoder) decodeBinary(n uint64) (interface{}, error) {
	if n > uint64(len(d.buf)) {
		return nil, errMsgpack
	}
	b, err := d.next(int(n))
	if err != nil {
		return nil, err
	}
	if utf8.Valid(b) && isPrintable(b) {
		return string(b), nil
	}
	return "0x" + hex.EncodeToString(b), nil
}

func (d *msgpackDecoder) decodeArray(n, depth int) (interface{}, error) {
	out := make([]interface{}, 0, min(n, 64))
	for i := 0; i < n; i++ {
		v, err := d.decodeValue(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (d *msgpackDecoder) decodeMap(n, depth int) (interface{}, error) {
	out := make(map[string]interface{}, min(n, 64))
	for i := 0; i < n; i++ {
		k, err := d.decodeValue(depth + 1)
		if err != nil {
			return nil, err
		}
		v, err := d.decodeValue(depth + 1)
		if err != nil {
			return nil, err
		}
		// JSON object keys must be strings; non-string keys are stringified
		// rather than rejected so that e.g. integer-keyed maps still render.
		out[mapKey(k)] = v
	}
	return out, nil
}

func mapKey(k interface{}) string {
	switch v := k.(type) {
	case string:
		return v
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
