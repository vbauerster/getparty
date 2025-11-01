package getparty

import (
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

func newFlashDecorator(decorator decor.Decorator, msgCh <-chan string, limit uint) decor.Decorator {
	if decorator == nil {
		return nil
	}
	d := &flashDecorator{
		Decorator: decorator,
		msgCh:     msgCh,
		limit:     limit,
	}
	return d
}

type flashDecorator struct {
	decor.Decorator
	msgCh <-chan string
	limit uint
	count uint
	msg   string
}

func (d *flashDecorator) Unwrap() decor.Decorator {
	return d.Decorator
}

func (d *flashDecorator) Decor(stat decor.Statistics) (string, int) {
	if d.count == 0 {
		select {
		case msg := <-d.msgCh:
			d.count = d.limit
			d.msg = msg
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
	var name string
	if atomic.LoadUint32(&globTry) != 0 {
		name = fmt.Sprintf("%s:R%02d", d.name, atomic.LoadUint32(d.curTry))
	} else {
		name = d.name
	}
	return d.Format(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

type peak struct {
	decor.WC
	format   string
	msg      string
	min      float64
	updCount uint8
	zDur     time.Duration
	mean     ewma.MovingAverage
}

func newSpeedPeak(format string, wc decor.WC) decor.Decorator {
	d := &peak{
		WC:     wc.Init(),
		format: format,
		mean:   ewma.NewMovingAverage(),
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
		return
	}
	d.zDur = 0
	d.mean.Add(durPerByte)
	if d.updCount == ewma.WARMUP_SAMPLES {
		mean := d.mean.Value()
		if d.min == 0 || mean < d.min {
			d.min = mean
		}
	} else {
		d.updCount++
	}
}

func (d *peak) Decor(stat decor.Statistics) (string, int) {
	if stat.Completed && d.msg == "" {
		if d.min == 0 {
			d.msg = "N/A"
		} else {
			d.msg = fmt.Sprintf(d.format, decor.FmtAsSpeed(decor.SizeB1024(math.Round(1e9/d.min))))
		}
	}
	return d.Format(d.msg)
}
