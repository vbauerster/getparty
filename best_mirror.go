package getparty

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"
)

type mirror struct {
	url    string
	sumDur time.Duration
	avgDur time.Duration
}

// bestMirror invariant: len(top) != 0 on err == nil
func (m Cmd) bestMirror(client *http.Client) (top []*mirror, err error) {
	var input io.Reader
	var fdClose func() error
	defer func() {
		if fdClose != nil {
			err = withStack(errors.Join(err, fdClose()))
		} else {
			err = withStack(err)
		}
	}()
	if m.opt.BestMirror.Mirrors == "-" {
		input = os.Stdin
	} else {
		fd, err := os.Open(m.opt.BestMirror.Mirrors)
		if err != nil {
			return nil, err
		}
		input, fdClose = fd, fd.Close
	}
	workers := cmp.Or(m.opt.BestMirror.MaxGo, uint(runtime.GOMAXPROCS(0)), 1)
	pass := cmp.Or(m.opt.BestMirror.Pass, 1)
	res, err := m.batchMirrors(input, client, workers, pass)
	if err != nil {
		return nil, err
	}
	top = <-res
	if len(top) == 0 {
		return nil, errors.New("none of the mirror has responded with valid result")
	}
	slices.SortFunc(top, func(a, b *mirror) int {
		return cmp.Compare(a.avgDur, b.avgDur)
	})
	return top, nil
}

func (m Cmd) batchMirrors(input io.Reader, client *http.Client, workers, pass uint) (<-chan []*mirror, error) {
	var eg errgroup.Group
	query := makeQueryFunc(m.Ctx, client, m.getTimeout())
	src, dst := readLines(m.Ctx, input), make(chan *mirror, workers)
	defer close(dst)

	m.loggers[DEBUG].Println("Best-mirror workers:", workers)
	m.loggers[DEBUG].Println("Best-mirror pass:", pass)

	for range workers {
		eg.Go(func() error {
			for mirror := range src {
				req, err := http.NewRequest(http.MethodHead, mirror.url, nil)
				if err != nil {
					m.loggers[WARN].Println(mirror.url, err.Error())
					continue
				}
				if m.patcher != nil {
					m.patcher(req)
				}
				var bad bool
				for i := uint(0); i < pass && !bad; i++ {
					// it's safe to reuse *http.Request here
					// https://github.com/golang/go/issues/19653#issuecomment-341540384
					dur, err := query(req)
					select {
					case <-m.Ctx.Done():
						return context.Cause(m.Ctx) // ^C by user most likely
					default:
						if err != nil {
							bad = true
							m.loggers[WARN].Println(mirror.url, unwrapOrErr(err).Error())
						} else {
							mirror.sumDur += dur
						}
					}
				}
				if !bad {
					avg := int64(mirror.sumDur) / int64(pass)
					mirror.avgDur = time.Duration(avg)
					dst <- mirror // send to dst is non blocking here
				}
			}
			return nil
		})
	}

	res := make(chan []*mirror, 1)
	go func() {
		var ss []*mirror
		for mirror := range dst {
			ss = append(ss, mirror)
		}
		res <- ss
	}()

	return res, eg.Wait()
}

func makeQueryFunc(ctx context.Context, client *http.Client, timeout time.Duration) func(*http.Request) (time.Duration, error) {
	return func(req *http.Request) (dur time.Duration, err error) {
		var resp *http.Response
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer func() {
			cancel()
			if err != nil {
				return
			}
			if resp.StatusCode != http.StatusOK {
				err = UnexpectedHttpStatus(resp.StatusCode)
			}
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}()
		start := time.Now()
		resp, err = client.Do(req.WithContext(ctx))
		return time.Since(start), err
	}
}

func readLines(ctx context.Context, r io.Reader) <-chan *mirror {
	ch := make(chan *mirror)
	go func() {
		defer close(ch)
		seen := make(map[string]bool)
		scanner := bufio.NewScanner(r)
		trim := func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		}
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimFunc(line, trim)
			if !strings.HasPrefix(line, "http") {
				continue
			}
			if !seen[line] {
				seen[line] = true
				m := &mirror{
					url: line,
				}
				select {
				case <-ctx.Done():
					return
				case ch <- m:
				}
			}
		}
		if err := scanner.Err(); err != nil {
			panic(err)
		}
	}()
	return ch
}
