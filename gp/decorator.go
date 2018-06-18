// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gp

import (
	"fmt"
	"unicode/utf8"

	"github.com/vbauerster/mpb/decor"
)

func countersDecorator(msgCh <-chan string, msgTimes int) decor.DecoratorFunc {
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
		counters := fmt.Sprintf("%.1f%% of % .1f", completed, decor.CounterKiB(s.Total))
		widthAccumulator <- utf8.RuneCountInString(counters)
		max := <-widthDistributor
		return fmt.Sprintf(fmt.Sprintf(format, max+1), counters)
	}
}

func percentage(total, current, ratio int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(ratio) * float64(current) / float64(total)
}
