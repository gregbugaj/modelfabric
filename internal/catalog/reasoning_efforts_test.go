package catalog

import (
	"slices"
	"testing"
)

// The levels belong to the model's chat template. llama.cpp's own help lists
// minimal, low, medium, high, xhigh and max; Qwen3.8-27B accepts three of them
// and answers 500 with a Jinja exception on the rest, so a fixed list in the
// settings form would break every request for four of six choices.
func TestReasoningEffortsComeFromTheTemplate(t *testing.T) {
	tpl := `{%- set reasoning_effort = reasoning_effort|default('xhigh') %}
{%- if reasoning_effort not in ('xhigh', 'medium', 'low') %}
{{- raise_exception('Unexpected reasoning effort ' ~ reasoning_effort) }}
{%- endif %}`
	got := parseReasoningEfforts(tpl)
	want := []string{"xhigh", "medium", "low"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A template that validates nothing tells us nothing, and "nothing" must not
// read as "no levels are allowed".
func TestReasoningEffortsUnknownWhenTheTemplateIsSilent(t *testing.T) {
	for _, tpl := range []string{
		"{%- if messages %}{{ messages[0].content }}{%- endif %}",
		"",
		"reasoning_effort is mentioned but never validated",
	} {
		if got := parseReasoningEfforts(tpl); got != nil {
			t.Errorf("template %q should yield no levels, got %v", tpl[:min(len(tpl), 30)], got)
		}
	}
}
