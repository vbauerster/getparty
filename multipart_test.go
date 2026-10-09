package getparty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type multipartTestWriter struct {
	delay     bool
	once      sync.Once
	mu        sync.Mutex
	logs      strings.Builder
	finished  map[string]bool
	truncate  string
	truncated bool
	hookErr   error
}

func (w *multipartTestWriter) Write(data []byte) (int, error) {
	text := string(data)
	if w.delay && strings.Contains(text, "[P01:R00]") && strings.Contains(text, "Response Status:") {
		w.once.Do(func() { time.Sleep(1500 * time.Millisecond) })
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.logs.WriteString(text)
	if w.truncate != "" && strings.Contains(text, "Return err: <nil>") {
		for _, part := range []string{"[P01:", "[P02:"} {
			if strings.Contains(text, part) {
				w.finished[part] = true
			}
		}
		if len(w.finished) == 2 && !w.truncated {
			w.truncated = true
			w.hookErr = os.Truncate(w.truncate, 1)
		}
	}
	return len(data), nil
}

func multipartTestServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRunRetriesTimeoutBeforeFirstCopy(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 8192)
	server := multipartTestServer(t, payload)
	destination := filepath.Join(t.TempDir(), "payload.bin")
	writer := &multipartTestWriter{delay: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := &Cmd{Ctx: ctx, Out: io.Discard, Err: writer}
	err := cmd.Run([]string{server.URL, "--parts=2", "--output.name=" + destination,
		"--output.overwrite", "--timeout=1", "--max-retry=1", "--debug"}, "", "")
	if err != nil {
		t.Fatalf("timeout must retry the incomplete part: %v\n%s", err, writer.logs.String())
	}
	actual, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, actual) {
		t.Fatalf("downloaded bytes differ: expected %d, got %d", len(payload), len(actual))
	}
	artifacts, err := filepath.Glob(destination + ".*")
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("completed download left artifacts: %v, %v", artifacts, err)
	}
}

func TestConcatenatePreservesIncompleteParts(t *testing.T) {
	dir := t.TempDir()
	session := &Session{AcceptRanges: "bytes", ContentLength: 2048}
	for i, written := range []int64{0, 1024} {
		name := filepath.Join(dir, fmt.Sprintf("payload.bin.%02d", i+1))
		payload := bytes.Repeat([]byte{byte(i)}, int(written))
		if err := os.WriteFile(name, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		output := &outFile{name: name}
		if err := output.Open(os.O_WRONLY | os.O_APPEND); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := output.Close(); err != nil {
				t.Error(err)
			}
		})
		session.Parts = append(session.Parts, &Part{
			Start: int64(i * 1024), Stop: int64((i+1)*1024 - 1), Written: written, output: output,
		})
	}
	progress := newProgress(context.Background(), io.Discard, io.Discard, 0)
	_, err := session.concatenate(progress, log.New(io.Discard, "", 0))
	progress.Wait()
	if err == nil {
		t.Fatal("incomplete part set must not be concatenated")
	}
	for i, part := range session.Parts {
		stat, err := os.Stat(part.output.Name())
		if err != nil || stat.Size() != part.Written {
			t.Fatalf("part %d was changed or deleted before validation: %v, %v", i+1, stat, err)
		}
	}
}

func TestConcatOpenFailureClosesEveryPart(t *testing.T) {
	dir := t.TempDir()
	parts := make([]*outFile, 4)
	for i := range parts {
		output := &outFile{name: filepath.Join(dir, fmt.Sprintf("payload.bin.%02d", i+1))}
		parts[i] = output
		if i == 3 {
			continue
		}
		if err := output.Open(os.O_WRONLY | os.O_CREATE | os.O_APPEND); err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(bytes.Repeat([]byte{byte(i)}, 1024)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if output.file != nil {
				if err := output.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Error(err)
				}
			}
		})
	}
	progress := newProgress(context.Background(), io.Discard, io.Discard, 0)
	bar, err := progress.addMergeBar(len(parts))
	if err != nil {
		t.Fatal(err)
	}
	err = concat(log.New(io.Discard, "", 0), bar, parts)
	bar.Abort(false)
	progress.Wait()
	if err == nil {
		t.Fatal("missing source part must fail concatenation")
	}
	for i, part := range parts {
		if part.file != nil {
			t.Errorf("part %d still has an open handle after concat returned", i+1)
		}
	}
}

func TestRunConcatFailureReleasesHandles(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 8192)
	server := multipartTestServer(t, payload)
	destination := filepath.Join(t.TempDir(), "payload.bin")
	writer := &multipartTestWriter{
		truncate: destination + ".01",
		finished: make(map[string]bool),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := &Cmd{Ctx: ctx, Out: io.Discard, Err: writer}
	err := cmd.Run([]string{server.URL, "--parts=2", "--output.name=" + destination,
		"--output.overwrite", "--debug"}, "", "")
	if !writer.truncated || writer.hookErr != nil {
		t.Fatalf("failed to inject a merge-length mismatch: %v", writer.hookErr)
	}
	if err == nil {
		t.Fatal("changed part contents must not produce a successful download")
	}
	for _, suffix := range []string{".01", ".02"} {
		name := destination + suffix
		if _, err := os.Stat(name); os.IsNotExist(err) {
			continue
		}
		if err := os.Remove(name); err != nil {
			t.Errorf("part handle is still held after Run returned: %v", err)
		}
	}
}

func TestRunDownloadModes(t *testing.T) {
	tests := []struct {
		name    string
		parts   int
		ranges  bool
		chunked bool
		empty   bool
	}{
		{name: "single", parts: 1, ranges: true},
		{name: "odd parts", parts: 3, ranges: true},
		{name: "ten parts", parts: 10, ranges: true},
		{name: "range fallback", parts: 3},
		{name: "unknown length", parts: 3, chunked: true},
		{name: "empty", parts: 1, ranges: true, empty: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("abcdefgh"), 8193)
			if test.empty {
				payload = nil
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.ranges {
					http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
					return
				}
				if test.chunked {
					w.(http.Flusher).Flush()
					if requests.Add(1) == 1 {
						<-r.Context().Done()
						return
					}
				} else {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				}
				if _, err := w.Write(payload); err != nil {
					t.Errorf("server write: %v", err)
				}
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "payload.bin")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := &Cmd{Ctx: ctx, Out: io.Discard, Err: io.Discard}
			err := cmd.Run([]string{server.URL, fmt.Sprintf("--parts=%d", test.parts),
				"--output.name=" + destination, "--output.overwrite"}, "", "")
			if err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(payload, actual) {
				t.Fatalf("download differs: expected %d bytes, got %d, error %v", len(payload), len(actual), err)
			}
		})
	}
}

func TestRunCompletedSinglePartResume(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 8192)
	server := multipartTestServer(t, payload)
	destination := filepath.Join(t.TempDir(), "payload.bin")
	session := &Session{
		URL: server.URL, OutputName: filepath.Base(destination),
		AcceptRanges: "bytes", ContentType: "application/octet-stream",
		ContentLength: int64(len(payload)), Single: true,
		Parts: []*Part{{Id: 1, Start: 0, Stop: int64(len(payload) - 1), Written: int64(len(payload))}},
	}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination+".json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination+".01", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := &Cmd{Ctx: ctx, Out: io.Discard, Err: io.Discard}
	if err := cmd.Run([]string{server.URL, "--parts=1", "--output.name=" + destination,
		"--output.overwrite"}, "", ""); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(payload, actual) {
		t.Fatalf("resumed download differs: got %d bytes, error %v", len(actual), err)
	}
}
