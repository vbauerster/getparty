package getparty

import (
	"bufio"
	"cmp"
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) {
	return len(p), nil
}

type mirror struct {
	url string
	dur time.Duration
}

// bestMirror invariant: len(top) != 0 on err == nil
func (m Cmd) bestMirror(transport http.RoundTripper, patcher httpRequestPatcher) (mirrors []*mirror, err error) {
	var input io.Reader
	if m.opt.BestMirror.Mirrors == "-" {
		input = os.Stdin
	} else {
		fd, fdErr := os.Open(m.opt.BestMirror.Mirrors)
		if fdErr != nil {
			return nil, fdErr
		}
		defer func() {
			clErr := fd.Close()
			m.loggers[DBUG].Printf("BestMirror: %q close: %v", m.opt.BestMirror.Mirrors, clErr)
			err = cmp.Or(err, clErr)
		}()
		input = fd
	}
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()
	mirrors, err = m.batchMirrors(client, patcher, input)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(mirrors, func(a, b *mirror) int {
		return cmp.Compare(a.dur, b.dur)
	})
	return mirrors, nil
}

func (m Cmd) batchMirrors(client *http.Client, patcher httpRequestPatcher, input io.Reader) ([]*mirror, error) {
	if m.opt.BestMirror.Doget != 0 {
		m.opt.BestMirror.Method = http.MethodGet
	}
	workers := cmp.Or(m.opt.BestMirror.Workers, uint(runtime.GOMAXPROCS(0)), 1)
	pass := cmp.Or(m.opt.BestMirror.Pass, 1)
	timeout := m.getBestMirrorTimeout()

	m.loggers[DBUG].Printf(
		"BestMirror: method=%s doget=%d workers=%d pass=%d timeout=%s",
		m.opt.BestMirror.Method,
		m.opt.BestMirror.Doget,
		workers,
		pass,
		timeout,
	)

	ctx, cancel := context.WithCancelCause(m.Ctx)
	defer cancel(nil)

	// need to force io.CopyBuffer to use buf; passing io.Discard as dst
	// will make buf nop because io.Discard implements io.ReaderFrom
	// therefore discard without io.ReaderFrom ensures buf will be used
	var dw io.Writer = discard{}
	query := func(req *http.Request, buf []byte) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return 0, UnexpectedHttpStatusError(resp.StatusCode)
		}
		if buf != nil {
			_, err = io.CopyBuffer(dw, io.LimitReader(resp.Body, int64(len(buf))), buf)
		}
		return time.Since(start), cmp.Or(err, resp.Body.Close())
	}

	var wg sync.WaitGroup
	downKiB := m.opt.BestMirror.Doget * 1024
	src := readLines(ctx, cancel, input, m.loggers)
	dst := make(chan *mirror, min(workers, 8))

	for range workers {
		wg.Go(func() {
			var buf []byte
			if downKiB != 0 {
				buf = make([]byte, downKiB)
			}
			for url := range src {
				req, err := http.NewRequest(m.opt.BestMirror.Method, url, nil)
				if err != nil {
					err = withMessage(err, "NewRequest")
					m.loggers[ERRO].Println(err.Error())
					m.loggers[DBUG].Println("BestMirror:", err.Error())
					cancel(err) // cancel all workers
					return
				}
				if patcher != nil {
					patcher.patch(req)
				}
				var sum time.Duration
				for range pass {
					// it's safe to reuse *http.Request: https://github.com/golang/go/issues/19653#issuecomment-341540384
					var dur time.Duration
					dur, err = query(req, buf)
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						m.loggers[WARN].Println(url, unwrapOrErr(err).Error())
						break
					}
					sum += dur
				}
				if err == nil {
					mir := &mirror{
						url: url,
						dur: time.Duration(int64(sum) / int64(pass)),
					}
					select {
					case dst <- mir:
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}

	go func() {
		wg.Wait()
		close(dst)
	}()

	var mirrors []*mirror
	for mir := range dst {
		mirrors = append(mirrors, mir)
	}
	if len(mirrors) == 0 {
		return nil, ErrMirrorNone
	}
	return mirrors, context.Cause(ctx)
}

func readLines(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	input io.Reader,
	loggers [lEVELS]*log.Logger,
) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		trim := func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		}
		seen := make(map[string]bool)
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimFunc(line, trim)
			if !strings.HasPrefix(line, "http") {
				loggers[WARN].Println("Invalid mirror:", line)
				continue
			}
			if !seen[line] {
				select {
				case ch <- line:
					seen[line] = true
				case <-ctx.Done():
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			err = withMessage(err, "Scan")
			loggers[ERRO].Println(err.Error())
			loggers[DBUG].Println("BestMirror:", err.Error())
			cancel(err)
		}
	}()
	return ch
}
