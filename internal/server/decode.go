package server

import (
	"encoding/json"
	"net/http"
)

// decodeBody preserves the permissive decoding used by management endpoints.
// Settings and presets use decodeStrict instead; sharing the read must not
// silently reject fields previously accepted by mixed-version clients.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
}
