package getparty

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vbauerster/mpb/v7/decor"
)

type message struct {
	times uint
	msg   string
	done  chan struct{}
}

type msgGate struct {
	msgCh    chan *message
	done     chan struct{}
	msgFlash func(*message)
}

func newMsgGate(quiet bool, prefix string, times uint) *msgGate {
	gate := &msgGate{
		msgCh: make(chan *message, 8),
		done:  make(chan struct{}),
		msgFlash: func(msg *message) {
			if msg.done != nil {
				close(msg.done)
				msg.done = nil
			}
		},
	}
	if !quiet {
		sinkFlash := gate.msgFlash
		gate.msgFlash = func(msg *message) {
			msg.times = times
			msg.msg = fmt.Sprintf("%s: %s", prefix, msg.msg)
			select {
			case gate.msgCh <- msg:
			case <-gate.done:
				sinkFlash(msg)
			}
		}
	}
	return gate
}

func (g *msgGate) flash(msg string) {
	g.msgFlash(&message{msg: msg})
}

func (g *msgGate) finalFlash(msg string) {
	flushed := make(chan struct{})
	g.msgFlash(&message{
		msg:  msg,
		done: flushed,
	})
	<-flushed
}

type mainDecorator struct {
	decor.WC
	curTry   *uint32
	name     string
	format   string
	finalMsg bool
	gate     *msgGate
	msg      *message
}

func newMainDecorator(curTry *uint32, format, name string, gate *msgGate, wc decor.WC) decor.Decorator {
	d := &mainDecorator{
		WC:     wc.Init(),
		curTry: curTry,
		name:   name,
		format: format,
		gate:   gate,
	}
	return d
}

func (d *mainDecorator) Shutdown() {
	close(d.gate.done)
}

func (d *mainDecorator) Decor(stat decor.Statistics) string {
	for d.msg == nil {
		select {
		case d.msg = <-d.gate.msgCh:
		default:
			name := d.name
			if atomic.LoadUint32(&globTry) > 0 {
				name = fmt.Sprintf("%s:R%02d", name, atomic.LoadUint32(d.curTry))
			}
			return d.FormatMsg(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
		}
	}
	switch {
	case d.finalMsg:
	case d.msg.done != nil:
		defer func() {
			close(d.msg.done)
			d.msg.done = nil
		}()
		d.finalMsg = true
	default:
		d.msg.times--
		if stat.Completed || d.msg.times == 0 {
			defer func() {
				d.msg = nil
			}()
		}
	}
	return d.FormatMsg(d.msg.msg)
}

type peak struct {
	decor.WC
	format  string
	msg     string
	counter bytesPerDuration
	min     bytesPerDuration
	once    sync.Once
}

type bytesPerDuration struct {
	bytes    int64
	duration int64
}

func newSpeedPeak(format string, wc decor.WC) decor.Decorator {
	d := &peak{
		WC:     wc.Init(),
		format: format,
		min:    bytesPerDuration{0, math.MaxInt64},
	}
	return d
}

func (s *peak) EwmaUpdate(n int64, dur time.Duration) {
	s.counter.bytes += n
	s.counter.duration += int64(dur)
	if s.counter.bytes >= 1024*128 {
		if s.counter.duration < s.min.duration {
			s.min = s.counter
		}
		s.counter = bytesPerDuration{0, 0}
	}
}

func (s *peak) onComplete() {
	if s.min.bytes == 0 {
		s.min = s.counter
	}
	if durPerByte := s.min.duration / s.min.bytes; durPerByte == 0 {
		s.msg = fmt.Sprintf(s.format, decor.FmtAsSpeed(decor.SizeB1024(0)))
	} else {
		s.msg = fmt.Sprintf(s.format, decor.FmtAsSpeed(decor.SizeB1024(1e9/durPerByte)))
	}
}

func (s *peak) Decor(stat decor.Statistics) string {
	if stat.Completed {
		s.once.Do(s.onComplete)
	}
	return s.FormatMsg(s.msg)
}
