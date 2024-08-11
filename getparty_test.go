package getparty_test

import (
	"testing"

	"github.com/vbauerster/getparty"
)

func TestParseContentDisposition(t *testing.T) {
	tests := []struct {
		input  string
		output string
	}{
		{
			input:  "attachment; filename=",
			output: "",
		},
		{
			input:  "attachment; filename=''",
			output: "",
		},
		{
			input:  `attachment; filename=""`,
			output: "",
		},
		{
			input:  "attachment; filename=content.txt",
			output: "content.txt",
		},
		{
			input:  "attachment; filename='content.txt'",
			output: "content.txt",
		},
		{
			input:  `attachment; filename="content.txt"`,
			output: "content.txt",
		},
		{
			input:  "attachment; filename*=UTF-8''content.txt",
			output: "content.txt",
		},
		{
			input:  "attachment; filename*=utf-8''%e2%82%ac%20rates",
			output: "€ rates",
		},
	}

	for _, test := range tests {
		output := getparty.ParseContentDisposition(test.input)
		if output != test.output {
			t.Errorf("expected %q got %q", test.output, output)
		}
	}
}
