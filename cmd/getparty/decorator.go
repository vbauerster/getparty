// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vbauerster/mpb/decor"
)

func countersDecorator(ch <-chan string, padding int) decor.DecoratorFunc {
	format := "%%%ds"
	var message string
	var current int64
	return func(s *decor.Statistics, myWidth chan<- int, maxWidth <-chan int) string {
		select {
		case message = <-ch:
			current = s.Current
		default:
		}

		if message != "" && current == s.Current {
			myWidth <- utf8.RuneCountInString(message)
			max := <-maxWidth
			return fmt.Sprintf(fmt.Sprintf(format, max+1), message)
		}

		completed := percentage(s.Total, s.Current, 100)
		counters := fmt.Sprintf("%.1f%% of % .2f", completed, decor.CounterKiB(s.Total))
		myWidth <- utf8.RuneCountInString(counters)
		max := <-maxWidth
		return fmt.Sprintf(fmt.Sprintf(format, max+1), counters)
	}
}

func speedDecorator(failure <-chan struct{}) decor.DecoratorFunc {
	var nowTime time.Time
	format := "%0.2f KiB/s"
	return func(s *decor.Statistics, myWidth chan<- int, maxWidth <-chan int) string {
		var str string
		select {
		case <-failure:
			str = strings.Replace(fmt.Sprintf(format, .0), "0", "-", -1)
		default:
		}

		if str == "" {
			if !s.Completed {
				nowTime = time.Now()
			}
			totTime := nowTime.Sub(s.StartTime)
			spd := float64(s.Current/1024) / totTime.Seconds()
			str = fmt.Sprintf(format, spd)
		}

		myWidth <- utf8.RuneCountInString(str)

		return fmt.Sprintf(fmt.Sprintf("%%%ds ", <-maxWidth), str)
	}
}

func percentage(total, current int64, ratio int) float64 {
	if total == 0 || current > total {
		return 0
	}
	return float64(ratio) * float64(current) / float64(total)
}
