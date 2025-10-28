package getparty

import (
	"bufio"
	"cmp"
	"container/heap"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"
)

var _ heap.Interface = (*mirrorPQ)(nil)

type mirror struct {
	index    int
	queryDur time.Duration
	url      string
}

type mirrorPQ []*mirror

func (pq mirrorPQ) Len() int { return len(pq) }

func (pq mirrorPQ) Less(i, j int) bool {
	return pq[i].queryDur < pq[j].queryDur
}

func (pq mirrorPQ) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *mirrorPQ) Push(x any) {
	s := *pq
	m := x.(*mirror)
	m.index = len(s)
	*pq = append(s, m)
}

func (pq *mirrorPQ) Pop() any {
	var m *mirror
	s := *pq
	i := len(s) - 1
	m, s[i] = s[i], nil // nil to avoid memory leak
	m.index = -1        // for safety
	*pq = s[:i]
	return m
}

func (m Cmd) bestMirror(transport http.RoundTripper) (top []*mirror, err error) {
	var input io.Reader
	fdClose := func() error { return nil }
	defer func() {
		err = withStack(errors.Join(err, fdClose()))
	}()
	if m.opt.BestMirror.Mirrors == "-" {
		input = os.Stdin
	} else {
		fd, err := os.Open(m.opt.BestMirror.Mirrors)
		if err != nil {
			return nil, err
		}
		input = fd
		fdClose = fd.Close
	}
	max := cmp.Or(m.opt.BestMirror.MaxGo, uint(runtime.GOMAXPROCS(0)), 1)
	res, err := m.batchMirrors(input, transport, max)
	if err != nil {
		return nil, err
	}
	pq := <-res
	if pq.Len() == 0 {
		return nil, errors.New("none of the mirror has responded with valid result")
	}
	for range cmp.Or(m.opt.BestMirror.TopN, uint(pq.Len())) {
		top = append(top, heap.Pop(&pq).(*mirror))
	}
	return top, nil
}

func (m Cmd) batchMirrors(input io.Reader, transport http.RoundTripper, workers uint) (<-chan mirrorPQ, error) {
	m.loggers[DEBUG].Println("Best-mirror max workers:", workers)

	src, dst := readLines(m.Ctx, input), make(chan *mirror)
	defer close(dst)
	eg, ctx := errgroup.WithContext(m.Ctx)
	timeout := m.getTimeout()
	client := &http.Client{Transport: transport}

	for range workers {
		eg.Go(func() error {
			for mirror := range src {
				err := m.queryMirror(mirror, client, timeout)
				if err != nil {
					if err := context.Cause(ctx); err != nil {
						return err // stop all workers
					}
					m.loggers[WARN].Println(mirror.url, unwrapOrErr(err).Error())
				} else {
					dst <- mirror
				}
			}
			return nil
		})
	}

	res := make(chan mirrorPQ, 1)
	go func() {
		var pq mirrorPQ
		for mirror := range dst {
			heap.Push(&pq, mirror)
		}
		res <- pq
	}()

	return res, eg.Wait()
}

func (m Cmd) queryMirror(mirror *mirror, client *http.Client, timeout time.Duration) error {
	req, err := http.NewRequest(http.MethodHead, mirror.url, nil)
	if err != nil {
		return err
	}
	if m.patcher != nil {
		m.patcher(req)
	}
	ctx, cancel := context.WithTimeout(m.Ctx, timeout)
	defer cancel()
	start := time.Now()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		mirror.queryDur = time.Since(start)
		return resp.Body.Close()
	}
	if resp.Body != nil {
		err = resp.Body.Close()
	}
	return errors.Join(err, UnexpectedHttpStatus(resp.StatusCode))
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
