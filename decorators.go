package getparty

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/vbauerster/mpb/v8/decor"
)

var (
	_ decor.Decorator     = (*mainDecorator)(nil)
	_ decor.Decorator     = (*flashDecorator)(nil)
	_ decor.Wrapper       = (*flashDecorator)(nil)
	_ decor.Decorator     = (*peak)(nil)
	_ decor.EwmaDecorator = (*peak)(nil)
)

type message struct {
	str   string
	final bool
}

type flashMessage struct {
	message
	count uint
}

func makeMsgHandler(ctx context.Context, quiet bool, msgCh chan<- message) func(message) {
	if quiet {
		return func(message) {}
	}
	ctx, cancel := context.WithCancel(ctx)
	send := func(msg message) {
		select {
		case msgCh <- msg:
		case <-ctx.Done():
		}
	}
	return func(msg message) {
		if msg.final {
			send(msg)
			cancel()
		} else {
			go send(msg)
		}
	}
}

func newFlashDecorator(decorator decor.Decorator, limit uint, msgCh <-chan message) decor.Decorator {
	if decorator == nil {
		return nil
	}
	d := &flashDecorator{
		Decorator: decorator,
		limit:     limit,
		msgCh:     msgCh,
	}
	return d
}

type flashDecorator struct {
	decor.Decorator
	limit   uint
	msgCh   <-chan message
	current flashMessage
}

func (d *flashDecorator) Unwrap() decor.Decorator {
	return d.Decorator
}

func (d *flashDecorator) Decor(stat decor.Statistics) (string, int) {
	if d.current.count == 0 {
		select {
		case msg := <-d.msgCh:
			if !msg.final {
				d.current.count = d.limit
			}
			d.current.message = msg
		default:
			return d.Decorator.Decor(stat)
		}
	}
	d.current.count--
	return d.Format(d.current.message.str)
}

type mainDecorator struct {
	decor.WC
	name   string
	format string
	curTry *uint32
}

func newMainDecorator(curTry *uint32, name, format string, wc decor.WC) decor.Decorator {
	d := &mainDecorator{
		WC:     wc.Init(),
		name:   name,
		format: format,
		curTry: curTry,
	}
	return d
}

func (d *mainDecorator) Decor(stat decor.Statistics) (string, int) {
	name := d.name
	if atomic.LoadUint32(&globTry) != 0 {
		name = fmt.Sprintf("%s:R%02d", name, atomic.LoadUint32(d.curTry))
	}
	return d.Format(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

type peak struct {
	decor.WC
	format    string
	msg       string
	min       float64
	completed bool
	mean      ewma.MovingAverage
}

func newSpeedPeak(format string, wc decor.WC) decor.Decorator {
	d := &peak{
		WC:     wc.Init(),
		format: format,
		mean:   ewma.NewMovingAverage(15),
	}
	return d
}

// EwmaUpdate will not be called by mpb if n == 0
func (s *peak) EwmaUpdate(n int64, dur time.Duration) {
	s.mean.Add(float64(dur) / float64(n))
	durPerByte := s.mean.Value()
	if s.min == 0 || durPerByte < s.min {
		s.min = durPerByte
	}
}

func (s *peak) Decor(stat decor.Statistics) (string, int) {
	if stat.Completed && !s.completed {
		if s.min == 0 {
			s.msg = "N/A"
		} else {
			s.msg = fmt.Sprintf(s.format, decor.FmtAsSpeed(decor.SizeB1024(math.Round(1e9/s.min))))
		}
		s.completed = true
	}
	return s.Format(s.msg)
}
