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

func TestMaxRedirect(t *testing.T) {
	tests := []struct {
		name string
		maxr string
		hops int
		err  error
	}{
		{
			name: "max-redirect 1, hops 1",
			maxr: "--max-redirect=1",
			hops: 1,
		},
		{
			name: "max-redirect 2, hops 2",
			maxr: "--max-redirect=2",
			hops: 2,
		},
		{
			name: "max-redirect 3, hops 3",
			maxr: "--max-redirect=3",
			hops: 3,
		},
		{
			name: "max-redirect 4, hops 4",
			maxr: "--max-redirect=4",
			hops: 4,
		},
		{
			name: "max-redirect 5, hops 5",
			maxr: "--max-redirect=5",
			hops: 5,
		},
		{
			name: "max-redirect 6, hops 6",
			maxr: "--max-redirect=6",
			hops: 6,
		},
		{
			name: "max-redirect 7, hops 7",
			maxr: "--max-redirect=7",
			hops: 7,
		},
		{
			name: "max-redirect 8, hops 8",
			maxr: "--max-redirect=8",
			hops: 8,
		},
		{
			name: "max-redirect 9, hops 9",
			maxr: "--max-redirect=9",
			hops: 9,
		},
		{
			name: "max-redirect default, hops 10",
			hops: 10,
		},
		{
			name: "max-redirect 0, hops 11",
			maxr: "--max-redirect=0",
			hops: 11,
		},
		{
			name: "max-redirect 1, hops 2",
			maxr: "--max-redirect=1",
			hops: 2,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 2, hops 3",
			maxr: "--max-redirect=2",
			hops: 3,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 3, hops 4",
			maxr: "--max-redirect=3",
			hops: 4,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 4, hops 5",
			maxr: "--max-redirect=4",
			hops: 5,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 5, hops 6",
			maxr: "--max-redirect=5",
			hops: 6,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 6, hops 7",
			maxr: "--max-redirect=6",
			hops: 7,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 7, hops 8",
			maxr: "--max-redirect=7",
			hops: 8,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 8, hops 9",
			maxr: "--max-redirect=8",
			hops: 9,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect 9, hops 10",
			maxr: "--max-redirect=9",
			hops: 10,
			err:  getparty.ErrMaxRedirect,
		},
		{
			name: "max-redirect default, hops 11",
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
