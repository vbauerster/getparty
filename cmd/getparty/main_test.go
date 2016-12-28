package main

import "testing"

func TestParseContentDisposition(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "",
			want:  "",
		},
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
		got := parseContentDisposition(test.input)
		if got != test.want {
			t.Errorf("Given: %s\nwant: %s\ngot: %s", test.input, test.want, got)
		}
	}
}

func TestCalcParts(t *testing.T) {
	tests := []struct {
		contentLength int64
		totalParts    int
		wantRange     map[int]string
	}{
		{
			contentLength: 1055406,
			totalParts:    0,
			wantRange:     map[int]string{1: "bytes=0-1055405"},
		},
		{
			contentLength: 1055406,
			totalParts:    1,
			wantRange:     map[int]string{1: "bytes=0-1055405"},
		},
		{
			contentLength: 1055406,
			totalParts:    2,
			wantRange: map[int]string{
				1: "bytes=0-527701",
				2: "bytes=527702-1055405",
			},
		},
		{
			contentLength: 1055406,
			totalParts:    3,
			wantRange: map[int]string{
				1: "bytes=0-351799",
				2: "bytes=351800-703602",
				3: "bytes=703603-1055405",
			},
		},
	}

	for _, test := range tests {
		al := &ActualLocation{
			ContentLength: test.contentLength,
		}
		al.calcParts(test.totalParts)
		for n, part := range al.Parts {
			got := part.getRange()
			want := test.wantRange[n]
			if got != want {
				t.Errorf("Given: %+v\nwant: %s\ngot: %s", test, want, got)
			}
		}
	}
}

func TestParseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://golang.org", "https://golang.org"},
		{"https://golang.org:443/test", "https://golang.org:443/test"},
		{"localhost:8080/test", "https://localhost:8080/test"},
		{"localhost:80/test", "http://localhost:80/test"},
		{"//localhost:8080/test", "https://localhost:8080/test"},
		{"//localhost:80/test", "http://localhost:80/test"},
	}

	for _, test := range tests {
		u := parseURL(test.in)
		if u.String() != test.want {
			t.Errorf("Given: %s\nwant: %s\ngot: %s", test.in, test.want, u.String())
		}
	}
}
