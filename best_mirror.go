package getparty

import (
	"bufio"
	"container/heap"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
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

func (cmd Cmd) bestMirror(maxGoroutines int, transport http.RoundTripper, args []string) ([]string, error) {
	var top []string
	var input io.Reader
	var fdClose func() error
	if len(args) != 0 {
		fd, err := os.Open(args[0])
		if err != nil {
			return nil, err
		}
		input = fd
		fdClose = fd.Close
	} else {
		input = os.Stdin
		fdClose = func() error { return nil }
	}
	pq := cmd.batchMirrors(input, transport, maxGoroutines)
	for i := 0; i < len(cmd.options.BestMirror) && pq.Len() != 0; i++ {
		m := heap.Pop(&pq).(*mirror)
		top = append(top, m.url)
		cmd.loggers[INFO].Printf("%s: %q", m.queryDur, m.url)
	}
	return top, fdClose()
}

func (cmd Cmd) batchMirrors(input io.Reader, transport http.RoundTripper, maxGoroutines int) mirrorPQ {
	mirrors := readLines(input)
	result := make(chan *mirror)

	timeout := cmd.getTimeout()
	var wg sync.WaitGroup
	wg.Add(maxGoroutines)
	go func() {
		wg.Wait()
		close(result)
	}()

	for i := 0; i < maxGoroutines; i++ {
		client := &http.Client{
			Transport: transport,
		}
		go func() {
			defer wg.Done()
			for m := range mirrors {
				err := queryMirror(cmd.Ctx, client, m, timeout)
				if err != nil {
					cmd.loggers[ERRO].Println(err)
					continue
				}
				result <- m
			}
		}()
	}

	var pq mirrorPQ
	for m := range result {
		heap.Push(&pq, m)
	}

	return pq
}

func queryMirror(
	ctx context.Context,
	client *http.Client,
	mirror *mirror,
	timeout time.Duration,
) error {
	req, err := http.NewRequest(http.MethodGet, mirror.url, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
		resp.Body.Close()
	}
	return HttpError(resp.StatusCode)
}

func readLines(r io.Reader) <-chan *mirror {
	ch := make(chan *mirror)
	go func() {
		seen := make(map[string]bool)
		scanner := bufio.NewScanner(r)
		index := 0
		for scanner.Scan() {
			line := scanner.Text()
			if len(line) == 0 || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimFunc(line, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsNumber(r)
			})
			if !seen[line] {
				seen[line] = true
				ch <- &mirror{
					index: index,
					url:   line,
				}
				index++
			}
		}
		close(ch)
		if err := scanner.Err(); err != nil {
			panic(err)
		}
	}()
	return ch
}
