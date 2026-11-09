package manager

import (
	"encoding/json"
	"regexp"
	"strings"

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

// agyErrorStatusWordRE accepts a status key only when the whole value is one
// canonical status word, so free text in that key never reaches a classifier.
var agyErrorStatusWordRE = regexp.MustCompile(`^[A-Z][A-Z_]*$`)

// agyErrorInfo is what agy-mcp keeps from an AGY_ERROR line. It deliberately has
// no field for short_error itself: that text carries the cloud project and
// region and is never copied into a status field (redactAgyErrorLines keeps it
// out of the stderr tail copied into Status.Error for lines that start with the
// prefix; see copyWindow for the over-long line gap).
type agyErrorInfo struct {
	retryable *bool  // nil: key absent
	errorID   string // "" when absent or failing validation
	status    string // canonical status word: short_error's prefix, else the line's own status key; "" when neither
}

// quotaWall reports whether the structured status is a quota wall. It is the one
// condition applyAgyError promotes on and markFinishedPrintTimeout defers to, so the
// two cannot drift apart.
func (i agyErrorInfo) quotaWall() bool { return isQuotaError(i.status) }

// agyErrorPayload is the wire shape of the object after the prefix. Status is the
// line's own status key (MEASURED against agy 1.5.3 on the print-timeout line).
type agyErrorPayload struct {
	ShortError string         `json:"short_error"`
	Status     agyErrorStatus `json:"status"`
	Retryable  *bool          `json:"retryable"`
	ErrorID    string         `json:"error_id"`
}

// agyErrorStatus decodes the line's status key leniently: a value that is not a
// JSON string leaves it empty instead of failing the whole line, so a retryable
// or error_id verdict is not lost to a status key of an unexpected type.
type agyErrorStatus string

func (s *agyErrorStatus) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = agyErrorStatus(v)
	}
	return nil
}

// parseAgyErrorTail extracts agy's structured error from the tail of a stderr
// file. truncated says the tail's first line is cut (the tail starts mid-line),
// so that line is dropped as a fragment; a tail that starts mid-file on a line
// boundary is not truncated. It reports ok == false on any doubt (no line, a cut
// or malformed object, a wrongly typed short_error, retryable or error_id, nothing
// usable), so a caller keeps its prior derivation unchanged. The status is the
// canonical word heading short_error, else the line's own status key; a status key
// of another type, or one that is not a single canonical word, counts as absent.
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
	} else if agyErrorStatusWordRE.MatchString(string(p.Status)) {
		// The prefix wins when both are present; the line's own status key is the
		// fallback for a short_error with no canonical prefix.
		info.status = string(p.Status)
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
// column 0, or after a bare carriage return when the whole line does not
// decode) reduced to the prefix plus the canonical status of its short_error,
// for example "AGY_ERROR: NOT_FOUND (code 404)", so the cloud project and
// region that short_error carries do not reach Status.Error through such a line
// (issue #205; copyWindow documents the over-long line gap). A line whose
// object does not decode, or whose short_error has no canonical status, is
// dropped; on the carriage-return path, a segment that does not decode drops
// the rest of its line, while text after a segment that decoded is kept. Other
// lines are kept as is.
func redactAgyErrorLines(tail string) string {
	if !strings.Contains(tail, agyErrorPrefix) {
		return tail
	}
	var out strings.Builder
	for line := range strings.Lines(tail) {
		if !strings.Contains(line, agyErrorPrefix) {
			out.WriteString(line)
			continue
		}
		body := strings.TrimRight(line, "\r\n")
		term := line[len(body):]
		// The whole line is decoded first: JSON allows a bare carriage return as
		// whitespace inside the object.
		if strings.HasPrefix(body, agyErrorPrefix) {
			if p, ok := decodeAgyErrorLine(body); ok {
				if m := agyErrorStatusRE.FindString(p.ShortError); m != "" {
					out.WriteString(agyErrorPrefix + strings.TrimSuffix(m, ":") + term)
				}
				continue
			}
		}
		// Otherwise a bare carriage return separates lines (progress output): each
		// CR-separated AGY_ERROR segment is reduced and the text around it kept. A
		// segment that does not decode drops the rest of the line, which may be the
		// rest of its object.
		var kept []string
		for seg := range strings.SplitSeq(body, "\r") {
			if !strings.HasPrefix(seg, agyErrorPrefix) {
				kept = append(kept, seg)
				continue
			}
			p, ok := decodeAgyErrorLine(seg)
			if !ok {
				break
			}
			if m := agyErrorStatusRE.FindString(p.ShortError); m != "" {
				kept = append(kept, agyErrorPrefix+strings.TrimSuffix(m, ":"))
			}
		}
		if len(kept) > 0 {
			out.WriteString(strings.Join(kept, "\r") + term)
		}
	}
	// A dropped last line must not leave a dangling newline.
	return trimStderrTail(out.String())
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
// MEASURED against agy 1.2.9, 1.3.1 and 1.5.3 for a model-not-found failure: exit 3, and
// the AGY_ERROR object has the keys short_error, retryable, error_id. MEASURED
// against agy 1.5.3 for an expired --print-timeout: exit 3, and the keys
// short_error, status ("DEADLINE_EXCEEDED"), retryable (false) and reason
// ("print_timeout"), plus tasks for a background-task expiry, with no error_id.
// Keys this decoder does not model are ignored. The canonical status is read from
// short_error's prefix, and from the line's own status key when short_error has no
// such prefix (the prefix wins when both are present). An unrecognized --model
// is a different, local failure: MEASURED against agy 1.5.3, an ERROR payload with
// finished_reason error and no AGY_ERROR line at all, so it gets no Retryable. A
// RESOURCE_EXHAUSTED status has NOT been captured; its promotion follows agy's
// canonical status naming, through quotaWall, the same predicate that keeps
// markFinishedPrintTimeout from relabelling such a run.
func applyAgyError(dir string, st Status) Status {
	info, ok := readAgyError(dir)
	if !ok {
		return st
	}
	st.Retryable = info.retryable
	st.ErrorID = info.errorID
	if info.quotaWall() {
		st.FailureReason = ReasonQuotaExhausted
	}
	return st
}
