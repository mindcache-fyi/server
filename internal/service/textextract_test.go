package service

import (
	"strings"
	"testing"
)

func TestExtractPlainText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "headings and emphasis stripped",
			in:   "# Title\n\nSome **bold** and _italic_ text.",
			want: "Title\n\nSome bold and italic text.",
		},
		{
			name: "links keep anchor drop url",
			in:   "See [the docs](https://example.com/a?b=1) now.",
			want: "See the docs now.",
		},
		{
			name: "images keep alt",
			in:   "![logo](https://example.com/logo.png)",
			want: "logo",
		},
		{
			name: "fence markers dropped, code kept",
			in:   "```go\nfmt.Println(\"hi\")\n```\nafter",
			want: "fmt.Println(\"hi\")\nafter",
		},
		{
			name: "list bullets removed",
			in:   "- alpha\n1. beta\n* gamma",
			want: "alpha\nbeta\ngamma",
		},
		{
			name: "cjk preserved",
			in:   "## 数据库\n\n连接池配置与**索引选择**。",
			want: "数据库\n\n连接池配置与索引选择。",
		},
		{
			name: "snake_case keeps underscores",
			in:   "use snake_case_names here",
			want: "use snake_case_names here",
		},
		{
			name: "escaped markdown char kept literally",
			in:   `not \*emphasis\* here`,
			want: "not *emphasis* here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractPlainText(tt.in); got != tt.want {
				t.Fatalf("ExtractPlainText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractPlainTextCollapsesWhitespace(t *testing.T) {
	got := ExtractPlainText("#  A   title\n\n\nwith\t\tgaps   inside")
	if strings.Contains(got, "  ") || strings.Contains(got, "\t") {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
}
