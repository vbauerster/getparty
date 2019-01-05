// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package getparty

import (
	"fmt"
	"strings"
	"sync"

	"github.com/vbauerster/mpb/decor"
)

type message struct {
	msg   string
	times int
	final bool
	done  chan struct{}
}

type percentageDecorator struct {
	decor.WC
	format   string
	done     chan struct{}
	mu       sync.Mutex
	messages []*message
	finalMsg *message
}

func percentageWithTotal(pairFormat string, wc decor.WC, ch <-chan *message) decor.Decorator {
	wc.Init()
	d := &percentageDecorator{
		WC:     wc,
		format: pairFormat,
		done:   make(chan struct{}),
	}
	go d.receive(ch)
	return d
}

func (d *percentageDecorator) receive(ch <-chan *message) {
	for {
		select {
		case m := <-ch:
			d.mu.Lock()
			if d.finalMsg == nil {
				d.messages = append(d.messages, m)
			}
			d.mu.Unlock()
		case <-d.done:
			return
		}
	}
}

func (d *percentageDecorator) Shutdown() {
	close(d.done)
}

func (d *percentageDecorator) Decor(stat *decor.Statistics) string {
	if d.finalMsg != nil {
		if d.finalMsg.times == 0 && d.finalMsg.done != nil {
			defer close(d.finalMsg.done)
			d.finalMsg.times++
		}
		return d.FormatMsg(d.finalMsg.msg)
	}

	d.mu.Lock()
	if len(d.messages) > 0 {
		m := d.messages[0]
		if m.times > 0 {
			m.times--
			d.mu.Unlock()
			return d.FormatMsg(m.msg)
		} else {
			if m.final {
				m.times = 0
				d.finalMsg = m
			}
			tmp := d.messages[:0]
			for i := 1; i < len(d.messages); i++ {
				tmp = append(tmp, d.messages[i])
			}
			d.messages = tmp
		}
	}
	d.mu.Unlock()

	completed := percentage(stat.Total, stat.Current, 100)
	msg := fmt.Sprintf(d.format, completed, decor.CounterKiB(stat.Total))
	return d.FormatMsg(msg)
}

func percentage(total, current, ratio int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(ratio*current) / float64(total)
}

func pad(diff int, wc decor.WC) decor.Decorator {
	wc.Init()
	return &padDecorator{
		WC:   wc,
		diff: diff,
	}
}

type padDecorator struct {
	decor.WC
	diff int
}

func (d *padDecorator) Decor(st *decor.Statistics) string {
	var max int
	if ok, ch := d.Syncable(); ok {
		ch <- 0
		max = <-ch
		max -= d.diff
	}
	return strings.Repeat(" ", max)
}
