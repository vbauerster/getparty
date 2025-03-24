package getparty

import (
	"bufio"
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

func (pq *mirrorPQ) Push(x interface{}) {
	n := len(*pq)
	link := x.(*mirror)
	link.index = n
	*pq = append(*pq, link)
}

func (pq *mirrorPQ) Pop() interface{} {
	old := *pq
	n := len(old)
	link := old[n-1]
	old[n-1] = nil  // avoid memory leak
	link.index = -1 // for safety
	*pq = old[:n-1]
	return link
}

func (m Cmd) bestMirror(transport http.RoundTripper) ([]string, error) {
	var top []string
	var input io.Reader
	var fdClose func() error
	if m.opt.BestMirror.Mirrors == "-" {
		input = os.Stdin
		fdClose = func() error { return nil }
	} else {
		fd, err := os.Open(m.opt.BestMirror.Mirrors)
		if err != nil {
			return nil, withStack(err)
		}
		input = fd
		fdClose = fd.Close
	}
	pq, err := m.batchMirrors(input, transport)
	if err != nil {
		return nil, withStack(err)
	}
	topn := int(m.opt.BestMirror.TopN)
	if topn == 0 {
		topn = pq.Len()
	}
	for i := 0; i < topn && pq.Len() != 0; i++ {
		mirror := heap.Pop(&pq).(*mirror)
		top = append(top, mirror.url)
		m.loggers[INFO].Printf("%s: %q", mirror.queryDur.Truncate(time.Microsecond), mirror.url)
	}
	return top, withStack(fdClose())
}

func (m Cmd) batchMirrors(input io.Reader, transport http.RoundTripper) (mirrorPQ, error) {
	max := int(m.opt.BestMirror.MaxGo)
	if max == 0 {
		max = runtime.NumCPU()
	}
	m.loggers[DEBUG].Println("Best-mirror max:", max)
	mirrors := readLines(m.Ctx, input)
	result := make(chan *mirror)
	defer close(result)

	eg, ctx := errgroup.WithContext(m.Ctx)
	timeout := m.getTimeout()
	client := &http.Client{
		Transport: transport,
	}

	for i := 0; i < max; i++ {
		eg.Go(func() error {
			for mirror := range mirrors {
				err := m.queryMirror(mirror, client, timeout)
				if err != nil {
					if ctx.Err() != nil {
						return context.Cause(ctx) // stop all workers
					} else {
						m.loggers[WARN].Println(err.Error())
					}
				} else {
					result <- mirror
				}
			}
			return nil
		})
	}

	var pq mirrorPQ
	go func() {
		for m := range result {
			heap.Push(&pq, m)
		}
	}()

	return pq, eg.Wait()
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
