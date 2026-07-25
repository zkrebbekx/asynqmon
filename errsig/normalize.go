// Package errsig implements the Fleet Console error-signature index (build
// contract §3.6/§5.7, phase 9): a leased background indexer with two feeders
// and one store —
//
//  1. an archived-tail feeder that score-cursors each queue's archived zset
//     (died-at ascending — append-only in score order, the ONLY state where
//     cursor-tailing is valid) and is therefore EXACT for everything it
//     observes;
//  2. a retry sampler that periodically re-reads a bounded window of each
//     queue's retry zset — explicitly NOT a cursor (retry scores are
//     future-dated next_process_at values, rewritten per attempt), so its
//     contributions are flagged sampled;
//
// plus a magnitude cross-check: today's failed counters (never trimmed) are
// stored alongside so the UI can show counter-derived magnitude next to
// index-derived identity. Counts sourced from a queue whose archive sits at
// the 10k trim cap are lower bounds, and the store says so.
package errsig

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// ****************************************************************************
// This file defines:
//   - Normalize: raw error message → signature template. Volatile fragments
//     collapse to placeholders so "connection refused to 10.0.0.7:5432" and
//     "connection refused to 10.0.0.9:5432" are one signature:
//       numbers            → ~N~   (unit suffixes survive: "after 30s" → "after ~N~s")
//       hex runs / UUIDs   → ~ID~
//       hostnames / IPs    → ~HOST~
//       quoted strings     → ~S~
//   - SigHash: the template's stable identity (truncated sha256, hex)
// ****************************************************************************

const (
	// maxRawLen bounds the normalizer's regexp work per message; anything
	// beyond it cannot plausibly change the signature identity.
	maxRawLen = 500

	// maxTemplateLen bounds the stored template (storage honesty: a 500-char
	// error prefix is already far more identity than a signature needs).
	maxTemplateLen = 240
)

// Replacement order is load-bearing and documented per rule below. Each rule
// runs over the whole string before the next starts.
var (
	// 1. UUIDs first: they contain hyphens and hex runs that rules 3/4 would
	// otherwise shred into partial replacements.
	uuidRE = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// 2. Hosts before hex: "deadbeef.example.com" must become one ~HOST~,
	// not "~ID~.example.com".
	//
	// 2a. IPv4 with optional port. Before the generic number rule so
	// "10.0.0.7:5432" never decays into "~N~.~N~.~N~.~N~:~N~".
	ipv4RE = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{1,5})?\b`)
	// 2b. Bracketed IPv6 (the URL form), optional port.
	ipv6RE = regexp.MustCompile(`\[[0-9a-fA-F:]+\](?::\d{1,5})?`)
	// 2c. Hostnames, three shapes — deliberately conservative so filenames
	// ("video.mp4") and versions ("2.41.0") survive as literals:
	//   - ≥3 dot-labels where the last label starts with a letter
	//     (api.stripe.com; excludes 2.41.0 whose last label is numeric)
	//   - 2 labels WITH a port (db-primary.internal:5432)
	//   - 2 labels whose TLD is on a short infra allowlist (redis.local)
	hostMultiRE = regexp.MustCompile(`\b(?:[A-Za-z0-9][A-Za-z0-9-]*\.){2,}[A-Za-z][A-Za-z0-9-]*(?::\d{1,5})?\b`)
	hostPortRE  = regexp.MustCompile(`\b[A-Za-z0-9][A-Za-z0-9-]*\.[A-Za-z0-9][A-Za-z0-9-]*:\d{1,5}\b`)
	hostTldRE   = regexp.MustCompile(`\b[A-Za-z0-9][A-Za-z0-9-]*\.(?:com|net|org|io|co|dev|app|cloud|internal|local|lan|corp|svc|cluster|edu|gov)\b`)

	// 3. Hex runs (optionally 0x-prefixed), ≥8 chars — SHAs, trace ids,
	// pointers. Word-bounded, so hex embedded in a longer alnum token is left
	// alone (better untouched than a half-replaced token).
	hexRE = regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{8,}\b`)

	// 4. Quoted strings. Double quotes and backticks match plainly; single
	// quotes only when the opening quote follows a boundary and the closing
	// quote precedes one, so apostrophes ("didn't", "user's") never pair up.
	dquoteRE = regexp.MustCompile(`"[^"]*"`)
	bquoteRE = regexp.MustCompile("`[^`]*`")
	squoteRE = regexp.MustCompile(`(^|[\s=:,(\[])'[^']*'($|[\s.,;:)\]])`)

	// 5. Numbers last, keeping any unit suffix so durations and sizes stay
	// readable ("after 30s" → "after ~N~s", "read 4096B" → "read ~N~B").
	numRE = regexp.MustCompile(`-?\b\d+(?:\.\d+)?(ns|us|µs|ms|s|m|h|d|[KMGT]i?B|B)?\b`)

	spaceRE = regexp.MustCompile(`\s+`)
)

// Normalize collapses one raw error message into its signature template.
// Deterministic, pure, and total: any string in, a bounded template out.
func Normalize(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) > maxRawLen {
		s = s[:maxRawLen]
	}
	s = spaceRE.ReplaceAllString(s, " ")

	s = uuidRE.ReplaceAllString(s, "~ID~")
	s = ipv6RE.ReplaceAllString(s, "~HOST~")
	s = ipv4RE.ReplaceAllString(s, "~HOST~")
	s = hostMultiRE.ReplaceAllString(s, "~HOST~")
	s = hostPortRE.ReplaceAllString(s, "~HOST~")
	s = hostTldRE.ReplaceAllString(s, "~HOST~")
	s = hexRE.ReplaceAllString(s, "~ID~")
	s = dquoteRE.ReplaceAllString(s, "~S~")
	s = bquoteRE.ReplaceAllString(s, "~S~")
	// The single-quote rule captures its boundary characters; keep them.
	s = squoteRE.ReplaceAllString(s, "${1}~S~${2}")
	s = numRE.ReplaceAllString(s, "~N~$1")

	s = strings.TrimSpace(s)
	if len(s) > maxTemplateLen {
		s = s[:maxTemplateLen]
	}
	return s
}

// SigHash is a template's stable identity: the first 16 bytes of its sha256,
// hex-encoded (32 chars — URL-safe, collision-safe at any plausible number of
// distinct signatures).
func SigHash(template string) string {
	sum := sha256.Sum256([]byte(template))
	return hex.EncodeToString(sum[:16])
}
