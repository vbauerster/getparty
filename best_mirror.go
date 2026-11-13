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

// bestMirror invariant: len([]*mirror) != 0 on err == nil
func (m Cmd) bestMirror(patcher httpRequestPatcher, transport http.RoundTripper) ([]*mirror, error) {
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()
	mirrors, err := batchMirrors(m.Ctx, patcher, client, m.opt, m.loggers)
	if err != nil {
		return nil, err
	}
	if len(mirrors) == 0 {
		return nil, ErrMirrorNone
	}
	slices.SortFunc(mirrors, func(a, b *mirror) int {
		return cmp.Compare(a.dur, b.dur)
	})
	return mirrors, nil
}

func batchMirrors(
	parent context.Context,
	patcher httpRequestPatcher,
	client *http.Client,
	opt *options,
	loggers [lEVELS]*log.Logger,
) (unsorted []*mirror, err error) {
	if opt.BestMirror.NoWarn {
		loggers[WARN] = loggers[DBUG]
	}
	if opt.BestMirror.Doget != 0 {
		opt.BestMirror.Method = http.MethodGet
	}
	workers := cmp.Or(opt.BestMirror.Workers, uint(runtime.GOMAXPROCS(0)), 1)
	pass := cmp.Or(opt.BestMirror.Pass, 1)
	timeout := time.Duration(min(cmp.Or(opt.BestMirror.Timeout, 5), maxTimeout)) * time.Second
	target := opt.BestMirror.Doget * 1024

	loggers[DBUG].Printf(
		"BestMirror options: method=%s doget=%d workers=%d pass=%d timeout=%s",
		opt.BestMirror.Method,
		target,
		workers,
		pass,
		timeout,
	)

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	query := func(req *http.Request) (time.Duration, error) {
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
		if target != 0 {
			_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, int64(target)))
		}
		return time.Since(start), cmp.Or(err, resp.Body.Close())
	}

	var input io.Reader
	if opt.BestMirror.Mirrors == "-" {
		input = os.Stdin
	} else {
		fd, er := os.Open(opt.BestMirror.Mirrors)
		if er != nil {
			return nil, er
		}
		defer func() {
			er := fd.Close()
			loggers[DBUG].Printf("BestMirror: %q close: %v", opt.BestMirror.Mirrors, er)
			err = cmp.Or(err, er)
		}()
		input = fd
	}

	var wg sync.WaitGroup
	src := readLines(ctx, cancel, input, loggers[:])
	dst := make(chan *mirror, min(workers, 8))

	for range workers {
		wg.Go(func() {
			for url := range src {
				req, err := http.NewRequest(opt.BestMirror.Method, url, nil)
				if err != nil {
					err = withMessage(err, "http.NewRequest")
					loggers[DBUG].Println("BestMirror:", err.Error())
					loggers[ERRO].Println(err.Error())
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
					dur, err = query(req)
					if err != nil {
						if ctx.Err() != nil {
							return
						}
						loggers[WARN].Println(url, unwrapOrErr(err).Error())
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

	for mir := range dst {
		unsorted = append(unsorted, mir)
	}
	return unsorted, context.Cause(ctx)
}

func readLines(
	parent context.Context,
	cancel context.CancelCauseFunc,
	input io.Reader,
	loggers []*log.Logger,
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
				case <-parent.Done():
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			err = withMessage(err, "Scan")
			loggers[DBUG].Println("BestMirror:", err.Error())
			loggers[ERRO].Println(err.Error())
			cancel(err)
		}
	}()
	return ch
}
