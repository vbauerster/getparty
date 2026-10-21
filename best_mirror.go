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
	mirrors, err = m.batchMirrors(&http.Client{Transport: transport}, patcher, input)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(mirrors, func(a, b *mirror) int {
		return cmp.Compare(a.dur, b.dur)
	})
	return mirrors, nil
}

func (m Cmd) batchMirrors(client *http.Client, patcher httpRequestPatcher, input io.Reader) ([]*mirror, error) {
	workers := cmp.Or(m.opt.BestMirror.MaxGo, uint(runtime.GOMAXPROCS(0)), 1)
	pass := cmp.Or(m.opt.BestMirror.Pass, 1)
	timeout := m.getBestMirrorTimeout()
	m.loggers[DBUG].Printf("BestMirror: workers=%d pass=%d timeout=%s", workers, pass, timeout)

	ctx, cancel := context.WithCancelCause(m.Ctx)
	defer cancel(nil)

	query := func(req *http.Request) (dur time.Duration, err error) {
		var resp *http.Response
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer func() {
			cancel()
			if err != nil {
				return
			}
			if resp.StatusCode != http.StatusOK {
				err = UnexpectedHttpStatusError(resp.StatusCode)
			}
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}()
		start := time.Now()
		resp, err = client.Do(req.WithContext(ctx))
		return time.Since(start), err
	}

	var wg sync.WaitGroup
	src := readLines(ctx, cancel, input, m.loggers)
	dst := make(chan *mirror, min(workers, 8))

	for range workers {
		wg.Go(func() {
			for url := range src {
				req, err := http.NewRequest(http.MethodHead, url, nil)
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
				mir := &mirror{url: url}
				for range pass {
					// it's safe to reuse *http.Request: https://github.com/golang/go/issues/19653#issuecomment-341540384
					var dur time.Duration
					dur, err = query(req)
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
					mir.dur = time.Duration(int64(sum) / int64(pass))
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
