package manager

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// agyErrorPrefix opens agy's structured error line on stderr (issue #183).
const agyErrorPrefix = "AGY_ERROR: "

// agyStatusResourceExhausted is the canonical status agy puts at the head of
// short_error for a quota or rate-limit wall. NOT MEASURED: no such sample has
// been captured; the mapping follows agy's canonical status naming.
const agyStatusResourceExhausted = "RESOURCE_EXHAUSTED"

// maxAgyErrorIDLen bounds an accepted error_id, so a hostile or garbled value
// never reaches the wire.
const maxAgyErrorIDLen = 128

// agyErrorStatusRE matches the canonical status word at the head of short_error,
// for example "NOT_FOUND (code 404): ...".
var agyErrorStatusRE = regexp.MustCompile(`^([A-Z][A-Z_]*) \(code \d+\):`)

// agyErrorInfo is what agy-mcp keeps from an AGY_ERROR line. It deliberately has
// no field for short_error itself: that text carries the cloud project and
// region and is never copied into a status field.
type agyErrorInfo struct {
	retryable *bool  // nil: key absent
	errorID   string // "" when absent or failing validation
	status    string // canonical status word from short_error's prefix, "" when none
}

// agyErrorPayload is the wire shape of the object after the prefix.
type agyErrorPayload struct {
	ShortError *string `json:"short_error"`
	Retryable  *bool   `json:"retryable"`
	ErrorID    *string `json:"error_id"`
}

// parseAgyErrorTail extracts agy's structured error from the tail of a stderr
// file. truncated says the tail starts mid-file, so its first line may be a
// fragment. It reports ok == false on any doubt (no line, a cut or malformed
// object, a wrongly typed field, nothing usable), so a caller keeps its prior
// derivation unchanged.
//
// Only the LAST line that starts with the prefix at column 0 is considered, with
// no fallback to an earlier one: an earlier line could be a superseded error.
func parseAgyErrorTail(tail string, truncated bool) (agyErrorInfo, bool) {
	if truncated {
		_, rest, found := strings.Cut(tail, "\n")
		if !found {
			return agyErrorInfo{}, false
		}
		tail = rest
	}
	var last string
	var seen bool
	for line := range strings.Lines(tail) {
		if strings.HasPrefix(line, agyErrorPrefix) {
			last, seen = line, true
		}
	}
	if !seen {
		return agyErrorInfo{}, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(last, agyErrorPrefix))
	// A leading brace rejects null, arrays and strings, which json.Unmarshal into
	// a struct would accept or half-accept.
	if !strings.HasPrefix(raw, "{") {
		return agyErrorInfo{}, false
	}
	var p agyErrorPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return agyErrorInfo{}, false
	}
	info := agyErrorInfo{retryable: p.Retryable}
	if p.ShortError != nil {
		if m := agyErrorStatusRE.FindStringSubmatch(*p.ShortError); m != nil {
			info.status = m[1]
		}
	}
	if p.ErrorID != nil && validAgyErrorID(*p.ErrorID) {
		info.errorID = *p.ErrorID
	}
	if info.retryable == nil && info.errorID == "" && info.status == "" {
		return agyErrorInfo{}, false
	}
	return info, true
}

// validAgyErrorID reports whether id is a short plain identifier.
func validAgyErrorID(id string) bool {
	if id == "" || len(id) > maxAgyErrorIDLen {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// readAgyError reads the job's stderr tail and parses it. It reads one byte more
// than errTailBytes, so a file of exactly errTailBytes is not mistaken for a cut
// one (tailFile returns exactly n bytes once the file is at least n long).
func readAgyError(dir string) (agyErrorInfo, bool) {
	raw, err := tailFile(jobstore.ErrPath(dir), errTailBytes+1)
	if err != nil {
		return agyErrorInfo{}, false
	}
	truncated := int64(len(raw)) > errTailBytes
	if truncated {
		// Keep the window at errTailBytes. A rune split by this cut can only sit in
		// the first line, which parseAgyErrorTail drops when truncated.
		raw = raw[1:]
	}
	return parseAgyErrorTail(raw, truncated)
}

// applyAgyError attaches agy's structured error verdict to a failed status st
// (issue #183). It sets Retryable and ErrorID, and promotes the reason to
// ReasonQuotaExhausted when the canonical status is RESOURCE_EXHAUSTED; it never
// changes any other reason, so a wording-matched quota wall is never demoted.
// It returns st unchanged when stderr has no usable line.
//
// MEASURED against agy 1.2.9 and 1.3.1: exit 3, two stderr lines, and the
// AGY_ERROR object has exactly the keys short_error, retryable, error_id. The
// RESOURCE_EXHAUSTED-to-quota promotion is NOT MEASURED.
func applyAgyError(dir string, st Status) Status {
	info, ok := readAgyError(dir)
	if !ok {
		return st
	}
	st.Retryable = info.retryable
	st.ErrorID = info.errorID
	if info.status == agyStatusResourceExhausted {
		st.FailureReason = ReasonQuotaExhausted
	}
	return st
}
