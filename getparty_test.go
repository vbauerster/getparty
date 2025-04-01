package getparty

import (
	"testing"
)

func TestMakeParts(t *testing.T) {
	tests := []struct {
		name      string
		n, length int64
		parts     [][2]int64
		err       error
	}{
		{
			name:   "0_33",
			n:      0,
			length: 33,
			err:    ErrZeroParts,
		},
		{
			name:   "8_33",
			n:      8,
			length: 33,
			err:    ErrTooFragmented,
		},
		{
			name:   "2_0",
			n:      2,
			length: 0,
			err:    ErrTooFragmented,
		},
		{
			name:   "2_-1",
			n:      2,
			length: -1,
			err:    ErrTooFragmented,
		},
		{
			name:   "1_0",
			n:      1,
			length: 0,
			parts:  [][2]int64{{0, -1}},
		},
		{
			name:   "1_-1",
			n:      1,
			length: -1,
			parts:  [][2]int64{{0, -2}},
		},
		{
			name:   "1_33",
			n:      1,
			length: 33,
			parts:  [][2]int64{{0, 32}},
		},
		{
			name:   "8_1024",
			n:      8,
			length: 1024,
			parts: [][2]int64{
				{0, 120},
				{121, 249},
				{250, 378},
				{379, 507},
				{508, 636},
				{637, 765},
				{766, 894},
				{895, 1023},
			},
		},
		{
			name:   "8_1025",
			n:      8,
			length: 1025,
			parts: [][2]int64{
				{0, 121},
				{122, 250},
				{251, 379},
				{380, 508},
				{509, 637},
				{638, 766},
				{767, 895},
				{896, 1024},
			},
		},
	}

	for _, test := range tests {
		parts, err := makeParts(test.n, test.length)
		if err != test.err {
			t.Errorf("%q: expected error %q got %q", test.name, test.err, err)
		}
		for i, p := range parts {
			x := test.parts[i]
			if start := x[0]; p.Start != start {
				t.Errorf("%q: expected start %d got %d", test.name, start, p.Start)
			}
			if stop := x[1]; p.Stop != stop {
				t.Errorf("%q: expected stop %d got %d", test.name, stop, p.Stop)
			}
		}
	}
}

func TestParseContentDisposition(t *testing.T) {
	tests := []struct {
		input  string
		output string
	}{
		{
			input:  "",
			output: "",
		},
		{
			input:  "garbage",
			output: "",
		},
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
			input:  "attachment; garbage=filename",
			output: "",
		},
		{
			input:  "attachment; filename=filename",
			output: "filename",
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
		output := parseContentDisposition(test.input)
		if output != test.output {
			t.Errorf("expected %q got %q", test.output, output)
		}
	}
}
