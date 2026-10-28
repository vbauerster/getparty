package getparty

import (
	"cmp"
	"container/heap"
	"errors"
	"log"
)

type chunkDatum struct {
	size    int64
	failure bool
}

type chunkScore struct {
	size     int64
	success  int
	failure  int
	weight   float64
	priority float64
	index    int
}

type priorityq []*chunkScore

func (pq priorityq) Len() int { return len(pq) }

func (pq priorityq) Less(i, j int) bool {
	return cmp.Less(pq[j].priority, pq[i].priority)
}

func (pq priorityq) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityq) Push(x any) {
	b := x.(*chunkScore)
	b.index = pq.Len()
	*pq = append(*pq, b)
}

func (pq *priorityq) Pop() any {
	old := *pq
	n := old.Len()
	pop := old[n-1]
	old[n-1] = nil // don't stop the GC from reclaiming the item eventually
	pop.index = -1 // for safety
	*pq = old[0 : n-1]
	return pop
}

func (pq *priorityq) update(item *chunkScore) {
	switch {
	case item.success != 0:
		total := item.success + item.failure
		rate := float64(item.success) / float64(total)
		item.priority = rate * item.weight
		heap.Fix(pq, item.index)
	case item.size != 8:
		item.priority = item.weight - 0.5
		heap.Fix(pq, item.index)
	}
}

type chunkScoreManager chan scoreManagerQuery

type scoreManagerCmd int

const (
	sm_put scoreManagerCmd = iota
	sm_get
)

type scoreManagerQuery struct {
	cmd  scoreManagerCmd
	data any
}

func (m chunkScoreManager) serve(logger *log.Logger) {
	chunks := map[int64]*chunkScore{
		8:  {weight: 1.00},
		16: {weight: 1.08},
		32: {weight: 1.16},
		64: {weight: 1.24},
	}

	pq := make(priorityq, len(chunks))
	i := 0
	for k, v := range chunks {
		v.size = k
		v.priority = v.weight
		v.index = i
		pq[i] = v
		i++
	}

	heap.Init(&pq)

	for req := range m {
		switch req.cmd {
		case sm_put:
			res := req.data.(chunkDatum)
			chunk, ok := chunks[res.size]
			if !ok {
				panic(errors.New("unknown chunk size"))
			}
			if res.failure {
				chunk.failure++
			} else {
				chunk.success++
			}
			pq.update(chunk)
		case sm_get:
			ch := req.data.(chan int64)
			ch <- pq[0].size
		}
	}

	for pq.Len() != 0 {
		logger.Printf("PQ: %+v", pq.Pop())
	}
}

func (m chunkScoreManager) put(item chunkDatum) {
	m <- scoreManagerQuery{cmd: sm_put, data: item}
}

func (m chunkScoreManager) get() chunkDatum {
	ch := make(chan int64)
	m <- scoreManagerQuery{cmd: sm_get, data: ch}
	return chunkDatum{size: <-ch}
}
