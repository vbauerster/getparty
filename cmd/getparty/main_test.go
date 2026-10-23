package main_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/vbauerster/getparty"
)

const expectedBody = "Hello, client"

func setupRedirectServer(source string, hops ...string) *httptest.Server {
	mux := http.NewServeMux()
	for _, next := range hops {
		mux.Handle(source, http.RedirectHandler(next, http.StatusFound))
		source = next
	}
	mux.HandleFunc(source, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, expectedBody)
	})
	return httptest.NewServer(mux)
}

func TestMaxFollow(t *testing.T) {
	tests := []struct {
		name string
		maxr string
		hops int
		err  error
	}{
		{
			name: "max-follow 1, hops 1",
			maxr: "--max-follow=1",
			hops: 1,
		},
		{
			name: "max-follow 2, hops 2",
			maxr: "--max-follow=2",
			hops: 2,
		},
		{
			name: "max-follow 3, hops 3",
			maxr: "--max-follow=3",
			hops: 3,
		},
		{
			name: "max-follow 4, hops 4",
			maxr: "--max-follow=4",
			hops: 4,
		},
		{
			name: "max-follow 5, hops 5",
			maxr: "--max-follow=5",
			hops: 5,
		},
		{
			name: "max-follow 6, hops 6",
			maxr: "--max-follow=6",
			hops: 6,
		},
		{
			name: "max-follow 7, hops 7",
			maxr: "--max-follow=7",
			hops: 7,
		},
		{
			name: "max-follow 8, hops 8",
			maxr: "--max-follow=8",
			hops: 8,
		},
		{
			name: "max-follow 9, hops 9",
			maxr: "--max-follow=9",
			hops: 9,
		},
		{
			name: "max-follow default, hops 10",
			hops: 10,
		},
		{
			name: "max-follow 0, hops 11",
			maxr: "--max-follow=0",
			hops: 11,
		},
		{
			name: "max-follow 1, hops 2",
			maxr: "--max-follow=1",
			hops: 2,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 2, hops 3",
			maxr: "--max-follow=2",
			hops: 3,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 3, hops 4",
			maxr: "--max-follow=3",
			hops: 4,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 4, hops 5",
			maxr: "--max-follow=4",
			hops: 5,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 5, hops 6",
			maxr: "--max-follow=5",
			hops: 6,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 6, hops 7",
			maxr: "--max-follow=6",
			hops: 7,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 7, hops 8",
			maxr: "--max-follow=7",
			hops: 8,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 8, hops 9",
			maxr: "--max-follow=8",
			hops: 9,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow 9, hops 10",
			maxr: "--max-follow=9",
			hops: 10,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-follow default, hops 11",
			hops: 11,
			err:  getparty.ErrMaxRedirect,
		},
	}

	ctx := context.Background()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var hops []string
			for i := range test.hops {
				hops = append(hops, "/"+strconv.Itoa(i+1))
			}
			ts := setupRedirectServer("/", hops...)
			defer ts.Close()
			var opts []string
			if test.maxr != "" {
				opts = append(opts, test.maxr)
			}
			output := t.TempDir() + "/test"
			opts = append(opts, "--output.overwrite", "--output.name="+output, ts.URL)
			cmd := &getparty.Cmd{
				Ctx: ctx,
				Out: io.Discard,
				Err: io.Discard,
			}
			err := cmd.Run(opts, "test", "")
			if test.err != nil {
				if !errors.Is(err, test.err) {
					t.Errorf("expected error %T got %v", test.err, err)
				}
			} else {
				if err != nil {
					t.Fatal("unexpected error", err)
				}
				b, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				if expectedBody != string(b) {
					t.Errorf("expected body %q got %q", expectedBody, string(b))
				}
			}
		})
	}
}
