package getparty

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/vbauerster/mpb/v4/decor"
)

type message struct {
	msg   string
	times int
	final bool
	done  chan struct{}
}

type msgGate struct {
	prefix string
	msgCh  chan *message
	done   chan struct{}
}

func newMsgGate(prefix string, quiet bool) msgGate {
	gate := msgGate{
		prefix: prefix,
		done:   make(chan struct{}),
	}
	if !quiet {
		gate.msgCh = make(chan *message, 4)
	}
	return gate
}

func (s msgGate) flash(msg *message) {
	msg.times = 14
	msg.msg = fmt.Sprintf("%s:%s", s.prefix, msg.msg)
	select {
	case s.msgCh <- msg:
	case <-s.done:
		if msg.final && msg.done != nil {
			close(msg.done)
		}
	}
}

type mainDecorator struct {
	decor.WC
	format   string
	name     string
	curTry   *int32
	flashMsg *message
	messages []*message
	gate     msgGate
}

func newMainDecorator(format, name string, curTry *int32, gate msgGate, wc decor.WC) decor.Decorator {
	wc.Init()
	d := &mainDecorator{
		WC:     wc,
		format: format,
		name:   name,
		curTry: curTry,
		gate:   gate,
	}
	return d
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
	if !stat.Completed && d.flashMsg != nil {
		m := d.flashMsg.msg
		if d.flashMsg.times > 0 {
			d.flashMsg.times--
		} else if d.flashMsg.final {
			if d.flashMsg.done != nil {
				close(d.flashMsg.done)
				d.flashMsg.done = nil
			}
		} else {
			d.flashMsg = nil
		}
		return d.FormatMsg(m)
	}

	d.depleteMessages()

	if len(d.messages) > 0 {
		d.flashMsg = d.messages[0]
		copy(d.messages, d.messages[1:])
		d.messages = d.messages[:len(d.messages)-1]
		d.flashMsg.times--
		return d.FormatMsg(d.flashMsg.msg)
	}

	name := fmt.Sprintf("%s:R%02d", d.name, atomic.LoadInt32(d.curTry))
	return d.FormatMsg(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

func (d *mainDecorator) Shutdown() {
	close(d.gate.done)
}

type speedMain struct {
	decor.Decorator
	average ewma.MovingAverage
	arec    decor.AmountReceiver
	format  string
	min     int64
	maxCh   chan string
	once    sync.Once
}

func newCompoundSpeed(format string, average ewma.MovingAverage, wc decor.WC) (main, complement decor.Decorator) {
	ch := make(chan string)
	decorator := decor.MovingAverageSpeed(decor.UnitKiB, format, average, wc)
	spm := &speedMain{
		Decorator: decorator,
		average:   average,
		arec:      decorator.(decor.AmountReceiver),
		format:    format,
		min:       math.MaxInt64,
		maxCh:     ch,
	}

	sdc := &speedComplement{
		WC:    wc.Init(),
		msgCh: ch,
	}

	return spm, sdc
}

func (spm *speedMain) NextAmount(n int64, wdd ...time.Duration) {
	spm.arec.NextAmount(n, wdd...)
}

func (spm *speedMain) onceOnComplete() {
	go func() {
		max := 1 / time.Duration(spm.min).Seconds()
		spm.maxCh <- fmt.Sprintf(
			spm.format,
			&decor.SpeedFormatter{decor.SizeB1024(math.Round(max))},
		)
	}()
}

func (spm *speedMain) Decor(st *decor.Statistics) string {
	if st.Completed {
		spm.once.Do(spm.onceOnComplete)
		return spm.Decorator.Decor(st)
	}
	if v := int64(math.Round(spm.average.Value())); v > 0 {
		if v < spm.min {
			spm.min = v
		}
	}
	return spm.Decorator.Decor(st)
}

type speedComplement struct {
	decor.WC
	once  sync.Once
	msgCh chan string
	msg   string
}

func (spc *speedComplement) getMsg() {
	spc.msg = <-spc.msgCh
}

func (spc *speedComplement) Decor(st *decor.Statistics) string {
	if st.Completed {
		spc.once.Do(spc.getMsg)
	}
	return spc.FormatMsg(spc.msg)
}
