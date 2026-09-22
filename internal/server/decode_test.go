package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagementDecodingPreservesEachEndpointsPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		strict     bool
		limit      int64
		wantErr    bool
	}{
		{"permissive accepts unknown fields", `{"name":"x","future":1}`, false, 1024, false},
		{"permissive keeps first-document behavior", `{"name":"x"}{}`, false, 1024, false},
		{"strict rejects unknown fields", `{"name":"x","future":1}`, true, 1024, true},
		{"strict rejects a second document", `{"name":"x"}{}`, true, 1024, true},
		{"strict accepts a valid object", `{"name":"x"}`, true, 1024, false},
		{"body limit is enforced", `{"name":"too long"}`, false, 8, true},
		{"malformed body fails", `{"name":`, false, 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst struct {
				Name string `json:"name"`
			}
			r := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var err error
			if tc.strict {
				err = decodeStrict(r, &dst)
			} else {
				err = decodeBody(httptest.NewRecorder(), r, &dst, tc.limit)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("decode error=%v, want error=%v", err, tc.wantErr)
			}
			if !tc.wantErr && dst.Name != "x" {
				t.Fatalf("decoded name=%q", dst.Name)
			}
		})
	}
}
