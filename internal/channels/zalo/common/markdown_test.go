package common

import "testing"

func TestStripMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "hello world", "hello world"},
		{"bold", "**bold**", "bold"},
		{"link", "[t](u)", "t (u)"},
		{"header", "# Title", "Title"},
		{"bullet", "- a\n- b", "• a\n• b"},
		{"fenced", "```\ncode\n```", "code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripMarkdown(tt.in); got != tt.want {
				t.Errorf("StripMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
