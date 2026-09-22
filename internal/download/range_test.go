package download

import (
	"net/http"
	"testing"
)

func TestValidateRangeRejectsAmbiguousOrInconsistentResponses(t *testing.T) {
	for _, tc := range []struct {
		name, header         string
		offset, size, length int64
		wantErr              bool
	}{
		{"exact remainder", "bytes 7-11/12", 7, 12, 5, false},
		{"chunked remainder", "bytes 7-11/12", 7, 12, -1, false},
		{"publisher size unknown", "bytes 7-11/12", 7, 0, 5, false},
		{"not a resume", "bytes 0-11/12", 0, 12, 12, true},
		{"trailing junk", "bytes 7-11/12 junk", 7, 12, 5, true},
		{"integer overflow", "bytes 7-9223372036854775808/12", 7, 12, 5, true},
		{"reversed range", "bytes 7-6/12", 7, 12, 0, true},
		{"end beyond total", "bytes 7-12/12", 7, 12, 6, true},
		{"incomplete remainder", "bytes 7-9/12", 7, 12, 3, true},
		{"unknown response total", "bytes 7-11/*", 7, 12, 5, true},
		{"wrong start", "bytes 6-11/12", 7, 12, 6, true},
		{"wrong published size", "bytes 7-12/13", 7, 12, 6, true},
		{"wrong body length", "bytes 7-11/12", 7, 12, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Response{Header: http.Header{"Content-Range": []string{tc.header}}, ContentLength: tc.length}
			if err := ValidateRange(r, tc.offset, tc.size); (err != nil) != tc.wantErr {
				t.Fatalf("got %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}
