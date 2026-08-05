package getparty

import (
	"cmp"
	"errors"
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

func newFlashDecorator(decorator decor.Decorator, msg string, signal <-chan struct{}) decor.Decorator {
	return newFlashDecoratorWithLimit(decorator, msg, signal, 0)
}

func newFlashDecoratorWithLimit(decorator decor.Decorator, msg string, signal <-chan struct{}, limit uint) decor.Decorator {
	if decorator == nil {
		return nil
	}
	d := &flashDecorator{
		Decorator: decorator,
		signal:    signal,
		limit:     cmp.Or(limit, 15),
		msg:       msg,
	}
	return d
}

type flashDecorator struct {
	decor.Decorator
	signal <-chan struct{}
	limit  uint
	count  uint
	msg    string
}

func (d *flashDecorator) Unwrap() decor.Decorator {
	return d.Decorator
}

func (d *flashDecorator) Decor(stat decor.Statistics) (string, int) {
	if d.count == 0 {
		select {
		case <-d.signal:
			d.count = d.limit
		default:
			return d.Decorator.Decor(stat)
		}
	} else {
		d.count--
	}
	return d.Format(d.msg)
}

type mainDecorator struct {
	decor.WC
	curTry *atomic.Uint32
	name   string
	format string
}

func newMainDecorator(curTry *atomic.Uint32, name, format string, wc decor.WC) decor.Decorator {
	if curTry == nil {
		panic(errors.New("expected non nil curTry"))
	}
	d := &mainDecorator{
		WC:     wc.Init(),
		curTry: curTry,
		name:   name,
		format: format,
	}
	return d
}

func (d *mainDecorator) Decor(stat decor.Statistics) (string, int) {
	var name string
	if globTry.Load() != 0 {
		name = fmt.Sprintf("%s:R%02d", d.name, d.curTry.Load())
	} else {
		name = d.name
	}
	return d.Format(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

type peak struct {
	decor.WC
	mean   ewma.MovingAverage
	format string
	msg    string
	min    float64
	zDur   time.Duration
}

func newSpeedPeak(format string, wc decor.WC) decor.Decorator {
	d := &peak{
		WC:     wc.Init(),
		mean:   decor.NewThreadSafeMovingAverage(ewma.NewMovingAverage(32)),
		format: format,
	}
	return d
}

func (d *peak) EwmaUpdate(n int64, dur time.Duration) {
	if n <= 0 {
		d.zDur += dur
		return
	}
	durPerByte := float64(d.zDur+dur) / float64(n)
	if math.IsInf(durPerByte, 0) || math.IsNaN(durPerByte) {
		d.zDur += dur
	} else {
		d.zDur = 0
		d.mean.Add(durPerByte)
	}
}

func (d *peak) Decor(stat decor.Statistics) (string, int) {
	if !stat.Completed {
		mean := d.mean.Value()
		if d.min == 0 || mean < d.min {
			d.min = mean
		}
		return d.Format("")
	}
	if d.min != 0 && d.msg == "" {
		d.msg = fmt.Sprintf(d.format, decor.FmtAsSpeed(decor.SizeB1024(math.Round(1e9/d.min))))
	}
	return d.Format(cmp.Or(d.msg, "N/A"))
}
