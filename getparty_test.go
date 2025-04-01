package getparty

import (
	"testing"
)

func TestMakeParts(t *testing.T) {
	tests := []struct {
		n, length int64
		parts     [][2]int64
		err       error
	}{
		{
			n:      0,
			length: 33,
			parts:  [][2]int64{},
			err:    ErrZeroParts,
		},
		{
			n:      8,
			length: 33,
			parts:  [][2]int64{},
			err:    ErrTooFragmented,
		},
		{
			n:      1,
			length: 33,
			parts:  [][2]int64{{0, 32}},
			err:    nil,
		},
		{
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
			err: nil,
		},
		{
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
			err: nil,
		},
	}

	for _, test := range tests {
		parts, err := makeParts(test.n, test.length)
		if err != test.err {
			t.Errorf("expected error %q got %q", test.err, err)
		}
		for i, p := range parts {
			x := test.parts[i]
			if start := x[0]; p.Start != start {
				t.Errorf("expected start %d got %d", start, p.Start)
			}
			if stop := x[1]; p.Stop != stop {
				t.Errorf("expected stop %d got %d", stop, p.Stop)
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
