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
	times int
	msg   string
	done  chan struct{}
}

type msgGate struct {
	msgCh    chan *message
	done     chan struct{}
	msgFlash func(*message)
}

func newMsgGate(quiet bool, prefix string, times int) *msgGate {
	gate := &msgGate{
		msgCh: make(chan *message, 1),
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
			msg.msg = fmt.Sprintf("%s:%s", prefix, msg.msg)
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
	messages []*message
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
	select {
	case m := <-d.gate.msgCh:
		if m.times > 0 {
			d.messages = append(d.messages, m)
		}
	default:
	}
	if len(d.messages) > 0 {
		m := d.messages[0]
	finalCheck:
		switch {
		case d.finalMsg:
		case m.times > 1:
			if stat.Completed {
				m.times = 0
				goto finalCheck
			}
			m.times--
		case m.done != nil:
			close(m.done)
			m.done = nil
			d.finalMsg = true
		default:
			copy(d.messages, d.messages[1:])
			d.messages = d.messages[:len(d.messages)-1]
		}
		return d.FormatMsg(m.msg)
	}

	name := d.name
	if atomic.LoadUint32(&globTry) > 0 {
		name = fmt.Sprintf("%s:R%02d", name, atomic.LoadUint32(d.curTry))
	}
	return d.FormatMsg(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

type peak struct {
	decor.WC
	format        string
	msg           string
	byteAcc       int64
	durAcc        int64
	minDurPerByte int64
	once          sync.Once
}

func newSpeedPeak(format string, wc decor.WC) decor.Decorator {
	d := &peak{
		WC:            wc.Init(),
		format:        format,
		minDurPerByte: math.MaxInt64,
	}
	return d
}

func (s *peak) EwmaUpdate(n int64, dur time.Duration) {
	s.byteAcc += n
	s.durAcc += int64(dur)
	if s.byteAcc > 1024*64 {
		durPerByte := s.durAcc / s.byteAcc
		if durPerByte == 0 {
			return
		}
		if durPerByte < s.minDurPerByte {
			s.minDurPerByte = durPerByte
		}
		s.byteAcc, s.durAcc = 0, 0
	}
}

func (s *peak) onComplete() {
	if s.byteAcc != 0 && s.minDurPerByte == 0 {
		durPerByte := s.durAcc / s.byteAcc
		if durPerByte != 0 && durPerByte < s.minDurPerByte {
			s.minDurPerByte = durPerByte
		}
	}
	if s.minDurPerByte == 0 {
		s.msg = fmt.Sprintf(s.format, decor.FmtAsSpeed(decor.SizeB1024(0)))
	} else {
		s.msg = fmt.Sprintf(s.format, decor.FmtAsSpeed(decor.SizeB1024(1e9/s.minDurPerByte)))
	}
}

func (s *peak) Decor(stat decor.Statistics) string {
	if stat.Completed {
		s.once.Do(s.onComplete)
	}
	return s.FormatMsg(s.msg)
}
