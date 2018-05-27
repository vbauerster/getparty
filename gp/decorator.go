// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gp

import (
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/vbauerster/mpb/decor"
)

func countersDecorator(msgCh <-chan string, msgTimes, padding int) decor.DecoratorFunc {
	format := "%%%ds"
	var message string
	var msgCount int
	return func(s *decor.Statistics, widthAccumulator chan<- int, widthDistributor <-chan int) string {
		select {
		case message = <-msgCh:
			msgCount = msgTimes
		default:
		}

		if msgCount != 0 {
			msgCount--
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
	format := "%0.2f KiB/s"
	return func(s *decor.Statistics, widthAccumulator chan<- int, widthDistributor <-chan int) string {
		spd := float64(s.Current/1024) / s.TimeElapsed.Seconds()
		if math.IsNaN(spd) || math.IsInf(spd, 0) {
			spd = .0
		}
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
