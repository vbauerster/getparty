package getparty

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vbauerster/mpb/v8"
)

func TestMakeParts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		length int64
		parts  [][2]int64
		err    error
	}{
		{
			name:   "0_33",
			length: 33,
			err:    ErrZeroParts,
		},
		{
			name:   "1_0",
			length: 0,
			parts:  [][2]int64{{0, -1}},
		},
		{
			name:   "1_-1",
			length: -1,
			parts:  [][2]int64{{0, -2}},
		},
		{
			name:   "1_1",
			length: 1,
			parts:  [][2]int64{{0, 0}},
		},
		{
			name:   "1_512",
			length: 512,
			parts:  [][2]int64{{0, 511}},
		},
		{
			name:   "1_512",
			length: 512,
			parts:  make([][2]int64, 2),
			err:    ErrTooFragmented,
		},
		{
			name:   "1_1024",
			length: 1024,
			parts:  [][2]int64{{0, 1023}},
		},
		{
			name:   "2_1024",
			length: 1024,
			parts: [][2]int64{
				{0, 511},
				{512, 1023},
			},
		},
		{
			name:   "3_1024",
			length: 1024,
			parts:  make([][2]int64, 3),
			err:    ErrTooFragmented,
		},
		{
			name:   "2_1025",
			length: 1025,
			parts: [][2]int64{
				{0, 511},
				{512, 1024},
			},
		},
		{
			name:   "2_2048",
			length: 2048,
			parts: [][2]int64{
				{0, 1023},
				{1024, 2047},
			},
		},
		{
			name:   "3_2048",
			length: 2048,
			parts: [][2]int64{
				{0, 681},
				{682, 1363},
				{1364, 2047},
			},
		},
		{
			name:   "4_2048",
			length: 2048,
			parts: [][2]int64{
				{0, 511},
				{512, 1023},
				{1024, 1535},
				{1536, 2047},
			},
		},
		{
			name:   "5_2048",
			length: 2048,
			parts:  make([][2]int64, 5),
			err:    ErrTooFragmented,
		},
		{
			name:   "4_2049",
			length: 2049,
			parts: [][2]int64{
				{0, 511},
				{512, 1023},
				{1024, 1535},
				{1536, 2048},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			session := &Session{
				AcceptRanges:  "bytes",
				ContentLength: test.length,
			}
			err := session.makeParts(uint(len(test.parts)))
			if test.err != nil {
				if !errors.Is(err, test.err) {
					t.Errorf("expected error %q got %q", test.err, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %q", err)
				}
				if len(session.Parts) != len(test.parts) {
					t.Errorf("expected len(parts)=%d got len(parts)=%d", len(test.parts), len(session.Parts))
				}
				for i, p := range session.Parts {
					x := test.parts[i]
					if start := x[0]; p.Start != start {
						t.Errorf("[%d] expected start %d got %d", i, start, p.Start)
					}
					if stop := x[1]; p.Stop != stop {
						t.Errorf("[%d] expected stop %d got %d", i, stop, p.Stop)
					}
				}
			}
		})
	}
}

func TestParseContentDisposition(t *testing.T) {
	t.Parallel()
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
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			output := parseContentDisposition(test.input)
			if output != test.output {
				t.Errorf("expected %q got %q", test.output, output)
			}
		})
	}
}

func TestParseOutputName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		location  string
		header    string
		expected  string
		pathFirst bool
	}{
		{
			location:  "",
			expected:  "",
			pathFirst: false,
		},
		{
			location:  "",
			expected:  "",
			pathFirst: true,
		},
		{
			location:  "http://exmaple.org",
			expected:  "",
			pathFirst: false,
		},
		{
			location:  "http://exmaple.org",
			expected:  "",
			pathFirst: true,
		},
		{
			location:  "http://exmaple.org/abc",
			expected:  "",
			pathFirst: false,
		},
		{
			location:  "http://exmaple.org/abc",
			expected:  "abc",
			pathFirst: true,
		},
		{
			location:  "http://exmaple.org/abc%20d",
			expected:  "",
			pathFirst: false,
		},
		{
			location:  "http://exmaple.org/abc%20d",
			expected:  "abc d",
			pathFirst: true,
		},
		{
			location:  "http://exmaple.org/abc",
			header:    "attachment; filename*=utf-8''%e2%82%ac%20rates",
			expected:  "€ rates",
			pathFirst: false,
		},
		{
			location:  "http://exmaple.org/abc",
			header:    "attachment; filename*=utf-8''%e2%82%ac%20rates",
			expected:  "abc",
			pathFirst: true,
		},
	}

	for _, test := range tests {
		name := fmt.Sprintf("PathFirst:%t;Location:%q", test.pathFirst, test.location)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := make(http.Header)
			h.Add(hContentDisposition, test.header)
			output, err := parseOutputName(test.location, h, test.pathFirst)
			if err != nil {
				t.Fatal(err)
			}
			if output != test.expected {
				t.Errorf("expected %q got %q", test.expected, output)
			}
		})
	}
}

func TestConcat(t *testing.T) {
	tests := []struct {
		parts    []*outFile
		nilfiles string
		expected string
	}{
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
			},
			expected: "12",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
			},
			nilfiles: "1",
			expected: "12",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
			},
			nilfiles: "2",
			expected: "12",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
			},
			nilfiles: "12",
			expected: "12",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "1",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "12",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "13",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "2",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "23",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "3",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
			},
			nilfiles: "123",
			expected: "123",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
				{name: "4"},
			},
			expected: "1234",
		},
		{
			parts: []*outFile{
				{name: "1"},
				{name: "2"},
				{name: "3"},
				{name: "4"},
				{name: "5"},
			},
			expected: "12345",
		},
	}

	logger := log.New(io.Discard, "", 0)
	progress := mpb.New(mpb.WithOutput(io.Discard))
	defer progress.Wait()

	for i, test := range tests {
		testname := fmt.Sprintf("%d:expected=%s:nilfiles=%s", i, test.expected, test.nilfiles)
		dir, err := os.MkdirTemp("", "getparty-concat")
		if err != nil {
			t.Fatal(err)
		}
		for _, of := range test.parts {
			basename := of.name
			of.name = filepath.Join(dir, basename)
			err := of.Open(os.O_WRONLY | os.O_CREATE)
			if err != nil {
				t.Fatal(err)
			}
			_, err = of.file.Write([]byte(basename))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(test.nilfiles, basename) {
				err := of.Close()
				if err != nil {
					t.Fatal(err)
				}
				of.file = nil
			}
		}
		t.Run(testname, func(t *testing.T) {
			bar := progress.AddBar(int64(len(test.parts)))
			defer bar.Abort(false)
			err := concat(logger, bar, test.parts)
			if err != nil {
				t.Fatal(err)
			}
			for _, of := range test.parts {
				err := of.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(test.parts[0].Name())
			if err != nil {
				t.Fatal(err)
			}
			if output := string(data); output != test.expected {
				t.Errorf("expected %q got %q", test.expected, output)
			}
			if !bar.Completed() {
				t.Fatal("expected completed bar")
			}
		})
	}
}
