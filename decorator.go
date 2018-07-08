// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package getparty

import (
	"fmt"
	"strings"

	"github.com/vbauerster/mpb/decor"
)

func percentageWithTotal(pairFormat string, wc decor.WC, msgCh <-chan string, msgTimes int) decor.Decorator {
	wc.Init()
	return &percentageDecorator{
		WC:       wc,
		format:   pairFormat,
		msgCh:    msgCh,
		msgTimes: msgTimes,
	}
}

type percentageDecorator struct {
	decor.WC
	format   string
	msg      string
	msgCh    <-chan string
	msgTimes int
	msgCount int
}

func (d *percentageDecorator) Decor(st *decor.Statistics) string {
	select {
	case d.msg = <-d.msgCh:
		d.msgCount = d.msgTimes
	default:
	}

	if d.msgCount > 0 {
		d.msgCount--
		return d.FormatMsg(d.msg)
	}

	completed := percentage(st.Total, st.Current, 100)
	counters := fmt.Sprintf(d.format, completed, decor.CounterKiB(st.Total))
	return d.FormatMsg(counters)
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
	if ok, ch := d.SyncWidth(); ok {
		ch <- 0
		max = <-ch
		max -= d.diff
	}
	return strings.Repeat(" ", max)
}
