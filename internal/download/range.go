// Package download holds HTTP checks shared by model and runtime downloads.
package download

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

var contentRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

// ValidateRange checks an open-ended resume response before the caller appends
// to its partial file. A 206 alone says nothing about which bytes were sent.
// Size may be zero when the publisher supplies a digest without a size.
func ValidateRange(resp *http.Response, offset, size int64) error {
	bad := func() error {
		return fmt.Errorf("invalid content-range %q for resume at %d of %d bytes; retry the download", resp.Header.Get("Content-Range"), offset, size)
	}
	parts := contentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if offset <= 0 || len(parts) != 4 {
		return bad()
	}
	start, err1 := strconv.ParseInt(parts[1], 10, 64)
	end, err2 := strconv.ParseInt(parts[2], 10, 64)
	total, err3 := strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || start != offset || end < start || end != total-1 || (size > 0 && total != size) {
		return bad()
	}
	if resp.ContentLength >= 0 && resp.ContentLength != end-start+1 {
		return bad()
	}
	return nil
}
