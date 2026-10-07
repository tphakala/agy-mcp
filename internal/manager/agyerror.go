package manager

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// agyErrorPrefix opens agy's structured error line on stderr (issue #183).
const agyErrorPrefix = "AGY_ERROR: "

// maxAgyErrorIDLen bounds an accepted error_id, so a hostile or garbled value
// never reaches the wire.
const maxAgyErrorIDLen = 128

// agyErrorStatusRE matches the canonical status word at the head of short_error,
// for example "NOT_FOUND (code 404): ...".
var agyErrorStatusRE = regexp.MustCompile(`^([A-Z][A-Z_]*) \(code \d+\):`)

// agyErrorInfo is what agy-mcp keeps from an AGY_ERROR line. It deliberately has
// no field for short_error itself: that text carries the cloud project and
// region and is never copied into a status field (redactAgyErrorLines keeps it
// out of the stderr tail copied into Status.Error).
type agyErrorInfo struct {
	retryable *bool  // nil: key absent
	errorID   string // "" when absent or failing validation
	status    string // canonical status word from short_error's prefix, "" when none
}

// agyErrorPayload is the wire shape of the object after the prefix.
type agyErrorPayload struct {
	ShortError string `json:"short_error"`
	Retryable  *bool  `json:"retryable"`
	ErrorID    string `json:"error_id"`
}

// parseAgyErrorTail extracts agy's structured error from the tail of a stderr
// file. truncated says the tail's first line is cut (the tail starts mid-line),
// so that line is dropped as a fragment; a tail that starts mid-file on a line
// boundary is not truncated. It reports ok == false on any doubt (no line, a cut or malformed
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
	for line := range strings.Lines(tail) {
		if strings.HasPrefix(line, agyErrorPrefix) {
			last = line
		}
	}
	if last == "" {
		return agyErrorInfo{}, false
	}
	p, ok := decodeAgyErrorLine(last)
	if !ok {
		return agyErrorInfo{}, false
	}
	info := agyErrorInfo{retryable: p.Retryable}
	if m := agyErrorStatusRE.FindStringSubmatch(p.ShortError); m != nil {
		info.status = m[1]
	}
	if validAgyErrorID(p.ErrorID) {
		info.errorID = p.ErrorID
	}
	if info.retryable == nil && info.errorID == "" && info.status == "" {
		return agyErrorInfo{}, false
	}
	return info, true
}

// decodeAgyErrorLine decodes one line that starts with agyErrorPrefix into its
// payload. It reports ok == false when the object after the prefix is not a
// complete JSON object.
func decodeAgyErrorLine(line string) (agyErrorPayload, bool) {
	raw := strings.TrimSpace(strings.TrimPrefix(line, agyErrorPrefix))
	// A leading brace rejects null, arrays and strings, which json.Unmarshal into
	// a struct would accept or half-accept.
	if !strings.HasPrefix(raw, "{") {
		return agyErrorPayload{}, false
	}
	var p agyErrorPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return agyErrorPayload{}, false
	}
	return p, true
}

// redactAgyErrorLines returns tail with every AGY_ERROR line (the prefix at
// column 0) reduced to the prefix plus the canonical status of its short_error,
// for example "AGY_ERROR: NOT_FOUND (code 404)", so the cloud project and region
// that short_error carries never reach Status.Error (issue #205). A line whose
// object does not decode, or whose short_error has no canonical status, is
// dropped. "Parses" here means both. Every other line is copied unchanged with
// its terminator. A prefix that is not at column 0 is not recognised, the same
// scope as parseAgyErrorTail, so such a line stays verbatim.
//
// dropHead drops the tail's first line through its first newline (all of it when
// there is none): the caller sets it when that line is the cut end of an
// AGY_ERROR line. Trailing whitespace is trimmed, so a dropped last line leaves
// no dangling newline, the rule cleanTail applies to every tail.
func redactAgyErrorLines(tail string, dropHead bool) string {
	if !dropHead && !strings.Contains(tail, agyErrorPrefix) {
		return tail
	}
	if dropHead {
		_, rest, found := strings.Cut(tail, "\n")
		if !found {
			return ""
		}
		tail = rest
	}
	var out strings.Builder
	for line := range strings.Lines(tail) {
		if !strings.HasPrefix(line, agyErrorPrefix) {
			out.WriteString(line)
			continue
		}
		body := strings.TrimRight(line, "\r\n")
		term := line[len(body):]
		p, ok := decodeAgyErrorLine(body)
		if !ok {
			continue
		}
		if m := agyErrorStatusRE.FindString(p.ShortError); m != "" {
			out.WriteString(agyErrorPrefix + strings.TrimSuffix(m, ":") + term)
		}
	}
	return strings.TrimRightFunc(out.String(), unicode.IsSpace)
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
// than errTailBytes and inspects that extra byte: a newline there means the
// errTailBytes window starts on a whole line, anything else means its first line
// is cut (tailFile returns exactly n bytes once the file is at least n long).
func readAgyError(dir string) (agyErrorInfo, bool) {
	raw, err := tailFile(jobstore.ErrPath(dir), errTailBytes+1)
	if err != nil {
		return agyErrorInfo{}, false
	}
	truncated := false
	if int64(len(raw)) > errTailBytes {
		// Keep the window at errTailBytes. A rune split by this cut can only sit in
		// the first line, which parseAgyErrorTail drops when truncated.
		truncated = raw[0] != '\n'
		raw = raw[1:]
	}
	return parseAgyErrorTail(raw, truncated)
}

// applyAgyError attaches agy's structured error verdict to a failed status st
// (issue #183): Retryable, ErrorID, and a promotion to ReasonQuotaExhausted when
// the canonical status reads as a quota wall (never a demotion). It returns st
// unchanged when stderr has no usable line.
//
// MEASURED against agy 1.2.9 and 1.3.1: exit 3, and the AGY_ERROR object has
// exactly the keys short_error, retryable, error_id. A RESOURCE_EXHAUSTED status
// has NOT been captured; its promotion follows agy's canonical status naming.
func applyAgyError(dir string, st Status) Status {
	info, ok := readAgyError(dir)
	if !ok {
		return st
	}
	st.Retryable = info.retryable
	st.ErrorID = info.errorID
	if isQuotaError(info.status) {
		st.FailureReason = ReasonQuotaExhausted
	}
	return st
}
