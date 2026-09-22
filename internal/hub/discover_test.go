package hub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuantOptionsGroupShardsAndAddTheProjector(t *testing.T) {
	opts := quantOptions([]File{
		{Name: "M-Q4_K_M.gguf", Size: 100},
		{Name: "M-Q8_0-00001-of-00002.gguf", Size: 150},
		{Name: "M-Q8_0-00002-of-00002.gguf", Size: 150},
		{Name: "mmproj-M-F16.gguf", Size: 10},
	})
	if len(opts) != 2 {
		t.Fatalf("options = %+v", opts)
	}
	q4, q8 := opts[0], opts[1]
	if q4.Quant != "Q4_K_M" || q4.Bytes != 110 || !q4.Recommended || q4.Projector != "mmproj-M-F16.gguf" {
		t.Errorf("Q4_K_M option = %+v", q4)
	}
	if q8.Quant != "Q8_0" || len(q8.Files) != 2 || q8.Bytes != 310 || q8.Recommended {
		t.Errorf("Q8_0 option = %+v (shards together, with the projector)", q8)
	}
}

func TestDetailsReadsFilesAndStripsReadmeFrontMatter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/"):
			io.WriteString(w, `{"sha":"abc","downloads":5,"likes":2,"pipeline_tag":"text-generation",
				"cardData":{"license":"apache-2.0","base_model":["Qwen/Qwen3-8B"]},
				"siblings":[{"rfilename":"README.md","size":3},{"rfilename":"Q-Q4_K_M.gguf","size":40}]}`)
		case strings.HasSuffix(r.URL.Path, "/README.md"):
			io.WriteString(w, "---\nlicense: apache-2.0\n---\n\n# Qwen3 8B\nHello.")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient()
	c.BaseURL = srv.URL
	d, err := c.Details(context.Background(), "lmstudio-community/Qwen3-8B-GGUF")
	if err != nil {
		t.Fatal(err)
	}
	if d.License != "apache-2.0" || d.BaseModel != "Qwen/Qwen3-8B" || len(d.Options) != 1 || d.Options[0].Bytes != 40 {
		t.Errorf("details = %+v", d)
	}
	if d.Readme != "# Qwen3 8B\nHello." {
		t.Errorf("readme = %q", d.Readme)
	}
	if _, err := c.Details(context.Background(), "not a repo"); err == nil {
		t.Error("an invalid repository name must be refused before any request")
	}
}

func TestModelInfoFromGGUFHeader(t *testing.T) {
	m := modelInfo("lmstudio-community", "Qwen/Qwen3.8-27B", ggufInfo{
		Total: 27320697856, Architecture: "qwen35", ContextLength: 262144,
		ChatTemplate: "{% if tools %}...{% endif %}<think>\n",
	})
	if m.Params != "27.3B" || m.Creator != "Qwen" || !m.Pick || !m.Tools || !m.Reasoning || m.ContextLength != 262144 {
		t.Errorf("model = %+v", m)
	}
	plain := modelInfo("bartowski", "", ggufInfo{Total: 596049920, ChatTemplate: "{{ messages }}"})
	if plain.Params != "596M" || plain.Creator != "bartowski" || plain.Pick || plain.Tools || plain.Reasoning {
		t.Errorf("plain = %+v", plain)
	}
	for n, want := range map[int64]string{7e9: "7B", 7518069290: "7.5B", 0: ""} {
		if got := paramsLabel(n); got != want {
			t.Errorf("paramsLabel(%d) = %q, want %q", n, got, want)
		}
	}
}

// A suffix check on the avatar host also accepted lookalikes.
func TestAvatarHostAllowlistIsNotASuffixMatch(t *testing.T) {
	for _, ok := range []string{"huggingface.co", "cdn-avatars.huggingface.co", "HuggingFace.co", "huggingface.co:443"} {
		if !allowedAvatarHost(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{"evil-huggingface.co", "huggingface.co.evil.com", "huggingface.com", "nothuggingface.co"} {
		if allowedAvatarHost(bad) {
			t.Errorf("%q must not be allowed", bad)
		}
	}
}
