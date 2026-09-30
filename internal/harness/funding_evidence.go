package harness

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
)

// FundingEvidence contains only allowlisted metadata. SourceTime is when the
// provider wrote a snapshot; observation time is added by the hook. Status is
// explicit so missing, malformed and inaccessible data never turn into zero.
//
// This file holds the harness-agnostic evidence machinery — the reader, the
// copiers and the value validators. Each provider's own evidence gathering lives
// in its file: Claude in claude_funding.go, Codex in codex_funding.go.
type FundingEvidence struct {
	Source     string
	Status     string
	SourceTime time.Time
	Attrs      map[string]any
}

// EvidenceFileLimit bounds an evidence file terma reads.
const EvidenceFileLimit = 2 << 20

// EvidenceLabel is what an evidence value copied as a label may look like.
var EvidenceLabel = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

// ReadEvidenceJSON reads a regular JSON object file within the bound, with the status
// "present"; when it cannot, the document is nil and the status says why: missing,
// unreadable, unsupported, oversized or malformed.
func ReadEvidenceJSON(path string) (map[string]json.RawMessage, string) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing"
	}
	if err != nil {
		return nil, "unreadable"
	}
	if !st.Mode().IsRegular() {
		return nil, "unsupported"
	}
	if st.Size() > EvidenceFileLimit {
		return nil, "oversized"
	}
	f, err := os.OpenFile(path, os.O_RDONLY|EvidenceOpenFlags, 0)
	if err != nil {
		return nil, "unreadable"
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, EvidenceFileLimit+1))
	if err != nil {
		return nil, "unreadable"
	}
	if len(data) > EvidenceFileLimit {
		return nil, "oversized"
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil || doc == nil {
		return nil, "malformed"
	}
	return doc, "present"
}

// CopyEvidenceString copies doc[from] to dst[to] when it is a bounded label.
func CopyEvidenceString(dst map[string]any, doc map[string]json.RawMessage, from, to string) {
	var value string
	if json.Unmarshal(doc[from], &value) == nil && EvidenceLabel.MatchString(value) {
		dst[to] = value
	}
}

// CopyEvidenceBool copies doc[from] to dst[to] when it is a boolean.
func CopyEvidenceBool(dst map[string]any, doc map[string]json.RawMessage, from, to string) {
	var value *bool
	if json.Unmarshal(doc[from], &value) == nil && value != nil {
		dst[to] = *value
	}
}

// CopyEvidenceNumber copies doc[from] to dst[to] when it is a finite number in
// [0, maxValue], and whole when integer is set.
func CopyEvidenceNumber(dst map[string]any, doc map[string]json.RawMessage, from, to string, maxValue float64, integer bool) {
	var n *float64
	if json.Unmarshal(doc[from], &n) == nil && n != nil && !math.IsNaN(*n) && !math.IsInf(*n, 0) && *n >= 0 && *n <= maxValue && (!integer || math.Trunc(*n) == *n) {
		dst[to] = *n
	}
}

// ValidEmail is a bounded shape check (not RFC 5322): the address rides verbatim into every quota
// spool entry, so it must be non-empty, within the spool budget, and free of separators that would
// corrupt an attribute value.
func ValidEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at >= len(s)-1 || strings.IndexByte(s[at+1:], '.') < 0 {
		return false
	}
	for _, c := range s {
		if c <= ' ' || c == '"' || c == ',' {
			return false
		}
	}
	return true
}

// ValidCreditBalance reports whether s is a decimal credit balance.
func ValidCreditBalance(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	dots, digits := 0, 0
	for _, c := range s {
		if c == '.' {
			dots++
		} else if c >= '0' && c <= '9' {
			digits++
		} else {
			return false
		}
	}
	return dots <= 1 && digits > 0
}
