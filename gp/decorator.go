// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gp

import (
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/vbauerster/mpb/decor"
)

func countersDecorator(msgCh <-chan string, padding int) decor.DecoratorFunc {
	format := "%%%ds"
	var message string
	var msgTimes int
	return func(s *decor.Statistics, widthAccumulator chan<- int, widthDistributor <-chan int) string {
		select {
		case message = <-msgCh:
			msgTimes = 3
		default:
		}

		if msgTimes != 0 {
			msgTimes--
			widthAccumulator <- utf8.RuneCountInString(message)
			max := <-widthDistributor
			return fmt.Sprintf(fmt.Sprintf(format, max+1), message)
		}

		completed := percentage(s.Total, s.Current, 100)
		counters := fmt.Sprintf("%.1f%% of % .2f", completed, decor.CounterKiB(s.Total))
		widthAccumulator <- utf8.RuneCountInString(counters)
		max := <-widthDistributor
		return fmt.Sprintf(fmt.Sprintf(format, max+1), counters)
	}
}

func speedDecorator() decor.DecoratorFunc {
	var nowTime time.Time
	format := "%0.2f KiB/s"
	return func(s *decor.Statistics, widthAccumulator chan<- int, widthDistributor <-chan int) string {
		if !s.Completed {
			nowTime = time.Now()
		}
		totTime := nowTime.Sub(s.StartTime)
		spd := float64(s.Current/1024) / totTime.Seconds()
		str := fmt.Sprintf(format, spd)

		widthAccumulator <- utf8.RuneCountInString(str)

		return fmt.Sprintf(fmt.Sprintf("%%%ds ", <-widthDistributor), str)
	}
}

func percentage(total, current, ratio int64) float64 {
	if total <= 0 {
		return 0
	}
	if current > total {
		current = total
	}
	return float64(ratio) * float64(current) / float64(total)
}
