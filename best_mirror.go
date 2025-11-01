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
	index  int
	sumDur time.Duration
	avgDur time.Duration
	url    string
}

// bestMirror invariant: len(top) != 0 on err == nil
func (m Cmd) bestMirror(transport http.RoundTripper) (top []*mirror, err error) {
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
	max := cmp.Or(m.opt.BestMirror.MaxGo, uint(runtime.GOMAXPROCS(0)), 1)
	res, err := m.batchMirrors(input, transport, max)
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

func (m Cmd) batchMirrors(input io.Reader, transport http.RoundTripper, workers uint) (<-chan []*mirror, error) {
	src, dst := readLines(m.Ctx, input), make(chan *mirror, workers)
	defer close(dst)

	var eg errgroup.Group
	timeout := m.getTimeout()
	client := &http.Client{Transport: transport}
	pass := cmp.Or(m.opt.BestMirror.Pass, 1)

	m.loggers[DEBUG].Println("Best-mirror workers:", workers)
	m.loggers[DEBUG].Println("Best-mirror pass:", pass)

	for range workers {
		eg.Go(func() error {
			for mirror := range src {
				var bad bool
				for i := uint(0); i < pass && !bad; i++ {
					err := mirror.query(m.Ctx, client, timeout, m.patcher)
					select {
					case <-m.Ctx.Done():
						return context.Cause(m.Ctx) // ^C by user most likely
					default:
						if err != nil {
							bad = true
							m.loggers[WARN].Println(mirror.url, unwrapOrErr(err).Error())
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

func (m *mirror) query(ctx context.Context, client *http.Client, timeout time.Duration, patcher func(*http.Request)) error {
	req, err := http.NewRequest(http.MethodHead, m.url, nil)
	if err != nil {
		return err
	}
	if patcher != nil {
		patcher(req)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	m.sumDur += time.Since(start)
	if resp.StatusCode == http.StatusOK {
		return resp.Body.Close()
	}
	err = UnexpectedHttpStatus(resp.StatusCode)
	if resp.Body == nil {
		return err
	}
	return errors.Join(err, resp.Body.Close())
}

func readLines(ctx context.Context, r io.Reader) <-chan *mirror {
	ch := make(chan *mirror)
	go func() {
		defer close(ch)
		index := 0
		seen := make(map[string]bool)
		scanner := bufio.NewScanner(r)
		trim := func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		}
		for scanner.Scan() {
			line := scanner.Text()
			if len(line) == 0 || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimFunc(line, trim)
			if !seen[line] {
				seen[line] = true
				m := &mirror{
					index: index,
					url:   line,
				}
				select {
				case ch <- m:
					index++
				case <-ctx.Done():
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			panic(err)
		}
	}()
	return ch
}
