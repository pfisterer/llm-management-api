package modeldesc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	d, err := Normalize(Description{Model: "  qwen3.6:35b  ", SourceURL: " https://huggingface.co/Qwen ", Description: "\n# Text\n"})
	if err != nil || d.Model != "qwen3.6:35b" || d.SourceURL != "https://huggingface.co/Qwen" || d.Description != "# Text" {
		t.Fatalf("%+v %v", d, err)
	}
	for name, bad := range map[string]Description{
		"no model":        {},
		"space in model":  {Model: "a b"},
		"long model":      {Model: strings.Repeat("m", MaxModelLen+1)},
		"javascript link": {Model: "m", SourceURL: "javascript:alert(1)"},
		"data link":       {Model: "m", SourceURL: "data:text/html,x"},
		"no host":         {Model: "m", SourceURL: "https://"},
		"long summary":    {Model: "m", Summary: strings.Repeat("s", MaxSummaryLen+1)},
		"long text":       {Model: "m", Description: strings.Repeat("d", MaxDescriptionLen+1)},
		"long licence":    {Model: "m", License: strings.Repeat("l", MaxShortLen+1)},
	} {
		var invalid *Invalid
		if _, err := Normalize(bad); !errors.As(err, &invalid) || invalid.Reason.IsZero() {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_ = s.Save(ctx, Description{Model: "b"})
	_ = s.Save(ctx, Description{Model: "a", License: "MIT"})
	_ = s.Save(ctx, Description{Model: "a", License: "Apache-2.0"})
	list, _ := s.List(ctx)
	if len(list) != 2 || list[0].Model != "a" || list[0].License != "Apache-2.0" {
		t.Fatalf("%+v", list)
	}
	_ = s.Delete(ctx, "a")
	_ = s.Delete(ctx, "missing")
	if list, _ := s.List(ctx); len(list) != 1 || ByModel(list)["b"].Model != "b" {
		t.Fatalf("%+v", list)
	}
}
