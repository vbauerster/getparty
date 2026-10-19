package getparty

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/VividCortex/ewma"
	"github.com/vbauerster/mpb/v8/decor"
)

const flashTimes = 16

var (
	_ decor.Decorator = (*mainDecorator)(nil)
	_ decor.Decorator = (*flashDecorator)(nil)
	_ decor.Decorator = (*ewmaSpeedPeak)(nil)
	_ decor.Wrapper   = (*ewmaSpeedPeak)(nil)
)

func newFlashDecorator(decorator *mainDecorator, msg string, signal <-chan struct{}) *flashDecorator {
	if decorator == nil {
		return nil
	}
	d := &flashDecorator{
		Decorator: decorator,
		signal:    signal,
		msg:       msg,
	}
	return d
}

type flashDecorator struct {
	decor.Decorator
	signal <-chan struct{}
	msg    string
	count  uint
}

func (d *flashDecorator) Decor(stat decor.Statistics) (string, int) {
	if d.count == 0 {
		select {
		case <-d.signal:
			d.count = flashTimes
		default:
			return d.Decorator.Decor(stat)
		}
	}
	d.count--
	return d.Format(d.msg)
}

type mainDecorator struct {
	decor.WC
	name   string
	format string
	curTry *atomic.Uint32
}

func newMainDecorator(curTry *atomic.Uint32, name, format string, wc decor.WC) *mainDecorator {
	if curTry == nil {
		panic(errors.New("expected non nil curTry"))
	}
	d := &mainDecorator{
		WC:     wc.Init(),
		name:   name,
		format: format,
		curTry: curTry,
	}
	return d
}

func (d *mainDecorator) Decor(stat decor.Statistics) (string, int) {
	var name string
	if globTry.Load() == 0 {
		name = d.name
	} else {
		name = fmt.Sprintf("%s:R%02d", d.name, d.curTry.Load())
	}
	return d.Format(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

type ewmaSpeedPeak struct {
	decor.Decorator
	mean   ewma.MovingAverage
	format string
	msg    string
	min    float64
}

func newEwmaSpeedPeak(format string, age float64, wc decor.WC) *ewmaSpeedPeak {
	mean := decor.NewThreadSafeMovingAverage(ewma.NewMovingAverage(age))
	d := &ewmaSpeedPeak{
		Decorator: decor.MovingAverageSpeed(decor.SizeB1024(0), format, mean, wc),
		mean:      mean,
		format:    format,
	}
	return d
}

// Unwrap is needed to expose EwmaUpdate(int64, time.Duration)
func (d *ewmaSpeedPeak) Unwrap() decor.Decorator {
	return d.Decorator
}

func (d *ewmaSpeedPeak) Decor(stat decor.Statistics) (string, int) {
	if !stat.Completed && !stat.Aborted {
		mean := d.mean.Value()
		if d.min == 0 || mean < d.min {
			d.min = mean
		}
		return d.Decorator.Decor(stat)
	}
	if d.min != 0 && d.msg == "" {
		d.msg = fmt.Sprintf(d.format, decor.FmtAsSpeed(decor.SizeB1024(math.Round(1e9/d.min))))
	}
	return d.Format(cmp.Or(d.msg, "N/A"))
}
