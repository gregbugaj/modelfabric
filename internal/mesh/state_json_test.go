package mesh

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStateJSONKeepsFlatFieldsAndUnknownValues(t *testing.T) {
	// Peers and dashboards consume these flat keys. Sharing the Go fields
	// must not introduce a nested stats object or change omitted/unknown data.
	for _, tc := range []struct {
		name  string
		state any
		input string
		want  string
	}{
		{
			name: "legacy engine keeps zero defaults", state: &EngineState{}, input: `{}`,
			want: `{"name":"","healthy":false,"models":null,"inflight":0,"kv_usage":0,"load_avg":0}`,
		},
		{
			name: "legacy instance keeps zero defaults", state: &InstanceState{}, input: `{}`,
			want: `{"id":"","model":"","source":"","runtime":"","kv_usage":0,"load_avg":0,"inflight":0,"address":"","port":0,"state":"","started":"0001-01-01T00:00:00Z"}`,
		},
		{
			name: "engine preserves unknown sentinels", state: &EngineState{},
			input: `{"name":"gpu","healthy":false,"models":null,"inflight":0,"spec_accepted":-1,"kv_usage":-1,"load_avg":-1}`,
		},
		{
			name: "instance preserves unknown sentinels", state: &InstanceState{},
			input: `{"id":"instance","model":"","source":"","runtime":"","kv_usage":-1,"load_avg":-1,"inflight":0,"address":"","port":0,"spec_accepted":-1,"state":"","started":"0001-01-01T00:00:00Z"}`,
		},
		{
			name: "engine preserves populated flat fields", state: &EngineState{},
			input: `{"name":"gpu","healthy":true,"models":["model"],"inflight":3,"slots":2,"context_length":8192,"kv_pool_tokens":16384,"context_note":"reported context","prefill_tok_s":1200,"decode_tok_s":60,"spec_accepted":0.75,"prefill_trusted":true,"prompt_tokens":9000,"cached_tokens":3000,"output_tokens":600,"kv_usage":0.25,"load_avg":2,"error":"engine diagnostic"}`,
		},
		{
			name: "instance preserves populated flat fields", state: &InstanceState{},
			input: `{"id":"instance","model":"model","source":"model","runtime":"runtime","engine":"mlx","served_model":"served-model","metrics_port":18001,"kv_usage":0.25,"load_avg":2,"inflight":3,"gpu":"gpu0","address":"127.0.0.1","port":18000,"slots":2,"context_length":8192,"kv_pool_tokens":16384,"context_note":"reported context","prefill_tok_s":1200,"decode_tok_s":60,"spec_accepted":0.75,"prefill_trusted":true,"prompt_tokens":9000,"cached_tokens":3000,"output_tokens":600,"vision":true,"vision_off":true,"state":"ready","pid":42,"error":"engine diagnostic","started":"2026-10-01T12:00:00Z"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.input), tc.state); err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(tc.state)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = tc.input
			}
			var gotFields, wantFields map[string]json.RawMessage
			if err := json.Unmarshal(got, &gotFields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(want), &wantFields); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotFields, wantFields) {
				t.Fatalf("wire JSON = %s, want %s", got, want)
			}
		})
	}
}
