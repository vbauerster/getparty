package main

import "testing"

func TestParseContentDisposition(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: `attachment;filename=""`,
			want:  "",
		},
		{
			input: `attachment;filename=''`,
			want:  "",
		},
		{
			input: `attachment;filename=content.txt`,
			want:  "content.txt",
		},
		{
			input: `attachment;filename="libtiff-4.0.7.sierra.bottle.tar.gz"`,
			want:  "libtiff-4.0.7.sierra.bottle.tar.gz",
		},
		{
			input: `attachment; filename="EURO rates"; filename*=utf-8''%e2%82%ac%20rates`,
			want:  "EURO rates",
		},
		{
			input: `attachment; filename*=utf-8''%e2%82%ac%20rates`,
			want:  "€ rates",
		},
		{
			input: `attachment;filename*=utf-8''%e2%82%ac%20rates;filenam="test.txt"`,
			want:  "€ rates",
		},
		{
			input: `attachment;filename*=UTF-8''Na%C3%AFve%20file.txt;`,
			want:  "Naïve file.txt",
		},
	}

	for _, test := range tests {
		got, err := parseContentDisposition(test.input)
		if err != nil {
			t.Error(err)
		}
		if got != test.want {
			t.Errorf("Given: %s\nwant: %s\ngot: %s", test.input, test.want, got)
		}
	}
}
