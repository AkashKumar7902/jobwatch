package htmltext

import (
	"strings"
	"testing"
)

func TestToText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string // substrings that must be present
	}{
		{
			"basic tags stripped",
			"<p>Hello <strong>world</strong></p>",
			[]string{"Hello", "world"},
		},
		{
			"greenhouse double-escaped html",
			"&lt;p&gt;Requires 1+ years of experience&lt;/p&gt;",
			[]string{"Requires 1+ years of experience"},
		},
		{
			"list items separated by newlines",
			"<ul><li>2 years experience</li><li>401k benefits</li></ul>",
			[]string{"2 years experience\n", "401k benefits"},
		},
		{
			"entities decoded",
			"<p>Design &amp; build</p>",
			[]string{"Design & build"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToText(tt.in)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("ToText(%q) = %q, missing %q", tt.in, got, want)
				}
			}
			if strings.Contains(got, "<") && strings.Contains(got, ">") {
				t.Errorf("ToText(%q) = %q still contains tags", tt.in, got)
			}
		})
	}
}
