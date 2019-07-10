package getparty

import (
	"fmt"
	"strings"
	"sync"

	"github.com/vbauerster/mpb/v4/decor"
)

type message struct {
	msg   string
	times int
	final bool
	done  chan struct{}
}

type msgGate struct {
	tryCh chan int
	msgCh chan *message
	quiet chan struct{}
	done  chan struct{}
}

func newMsgGate(quiet bool) msgGate {
	gate := msgGate{
		tryCh: make(chan int),
		msgCh: make(chan *message, 2),
		done:  make(chan struct{}),
	}
	if quiet {
		gate.tryCh = nil
		gate.msgCh = nil
		gate.quiet = make(chan struct{})
		close(gate.quiet)
	}
	return gate
}

func (s msgGate) flash(msg *message) {
	msg.times = 14
	select {
	case s.msgCh <- msg:
	case <-s.quiet:
		if msg.final && msg.done != nil {
			close(msg.done)
		}
	case <-s.done:
		if msg.final && msg.done != nil {
			close(msg.done)
		}
	}
}

func (s msgGate) setRetry(r int) {
	select {
	case s.tryCh <- r:
	case <-s.quiet:
	case <-s.done:
	}
}

type mainDecorator struct {
	decor.WC
	name     string
	format   string
	msg      *message
	messages []*message
	gate     msgGate

	mu    sync.RWMutex
	retry int
}

func newMainDecorator(format, name string, gate msgGate, wc decor.WC) decor.Decorator {
	wc.Init()
	d := &mainDecorator{
		WC:     wc,
		name:   name,
		format: format,
		gate:   gate,
	}
	go d.receiveTries()
	return d
}

func (d *mainDecorator) receiveTries() {
	for {
		select {
		case r := <-d.gate.tryCh:
			d.mu.Lock()
			d.retry = r
			d.mu.Unlock()
		case <-d.gate.done:
			return
		}
	}
}

func (d *mainDecorator) depleteMessages() {
	for {
		select {
		case m := <-d.gate.msgCh:
			d.messages = append(d.messages, m)
		default:
			return
		}
	}
}

func (d *mainDecorator) Decor(stat *decor.Statistics) string {
	if d.msg != nil {
		m := d.msg.msg
		if d.msg.times > 0 {
			d.msg.times--
		} else if d.msg.final {
			if d.msg.done != nil {
				close(d.msg.done)
				d.msg.done = nil
			}
		} else {
			d.msg = nil
		}
		return d.FormatMsg(m)
	}

	d.depleteMessages()

	if len(d.messages) > 0 {
		d.msg = d.messages[0]
		copy(d.messages, d.messages[1:])
		d.messages = d.messages[:len(d.messages)-1]
		d.msg.times--
		return d.FormatMsg(d.msg.msg)
	}

	var sb strings.Builder
	sb.WriteString(d.name)
	d.mu.RLock()
	sb.WriteString(fmt.Sprintf(":R%02d", d.retry))
	d.mu.RUnlock()
	return d.FormatMsg(fmt.Sprintf(d.format, sb.String(), decor.CounterKiB(stat.Total)))
}

func (d *mainDecorator) Shutdown() {
	close(d.gate.done)
}
