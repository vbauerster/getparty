// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package getparty

import (
	"fmt"
	"sync"

	"github.com/vbauerster/mpb/v4/decor"
)

type msgGate struct {
	msgCh chan *message
	done  chan struct{}
}

type message struct {
	msg        string
	flashTimes int
	final      bool
	done       chan struct{}
}

type percentageDecorator struct {
	decor.WC
	format   string
	gate     msgGate
	msg      *message
	messages []*message
	mu       sync.Mutex
	entry    []*message
}

func newPercentageWithTotal(pairFormat string, g msgGate, wc decor.WC) decor.Decorator {
	wc.Init()
	d := &percentageDecorator{
		WC:     wc,
		format: pairFormat,
		gate:   g,
	}
	go d.receive()
	return d
}

func (d *percentageDecorator) receive() {
	for {
		select {
		case m := <-d.gate.msgCh:
			d.mu.Lock()
			d.entry = append(d.entry, m)
			d.mu.Unlock()
		case <-d.gate.done:
			return
		}
	}
}

func (d *percentageDecorator) Decor(stat *decor.Statistics) string {
	if d.msg != nil {
		if d.msg.final || d.msg.flashTimes > 0 {
			d.msg.flashTimes--
			return d.FormatMsg(d.msg.msg)
		}
	}

	d.mu.Lock()
	if len(d.entry) > 0 {
		d.messages = append(d.messages, d.entry[0])
		copy(d.entry, d.entry[1:])
		d.entry = d.entry[:len(d.entry)-1]
	}
	d.mu.Unlock()

	if len(d.messages) > 0 {
		d.msg = d.messages[0]
		copy(d.messages, d.messages[1:])
		d.messages = d.messages[:len(d.messages)-1]
		if d.msg.done != nil {
			close(d.msg.done)
		}
		if d.msg.flashTimes > 0 {
			d.msg.flashTimes--
			return d.FormatMsg(d.msg.msg)
		}
	}

	completed := percentage(stat.Total, stat.Current, 100)
	msg := fmt.Sprintf(d.format, completed, decor.CounterKiB(stat.Total))
	return d.FormatMsg(msg)
}

func (d *percentageDecorator) Shutdown() {
	close(d.gate.done)
}

func percentage(total, current, ratio int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(ratio*current) / float64(total)
}

type tryGate struct {
	msgCh chan string
	done  chan struct{}
}

type triesDecorator struct {
	decor.WC
	gate tryGate
	mu   sync.RWMutex
	msg  string
}

func newTriesDecorator(g tryGate, wc decor.WC) decor.Decorator {
	wc.Init()
	d := &triesDecorator{
		WC:   wc,
		gate: g,
	}
	go d.receive()
	return d
}

func (d *triesDecorator) receive() {
	for {
		select {
		case m := <-d.gate.msgCh:
			d.mu.Lock()
			d.msg = m
			d.mu.Unlock()
		case <-d.gate.done:
			return
		}
	}
}

func (d *triesDecorator) Decor(_ *decor.Statistics) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.FormatMsg(d.msg)
}

func (d *triesDecorator) Shutdown() {
	close(d.gate.done)
}
