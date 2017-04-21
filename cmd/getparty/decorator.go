// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vbauerster/mpb"
)

type barSlice []*mpb.Bar

func (bs barSlice) Len() int { return len(bs) }

func (bs barSlice) Less(i, j int) bool {
	return bs[i].GetID() < bs[j].GetID()
}

func (bs barSlice) Swap(i, j int) { bs[i], bs[j] = bs[j], bs[i] }

func sortByBarNameFunc() mpb.BeforeRender {
	return func(bars []*mpb.Bar) {
		sort.Sort(barSlice(bars))
	}
}

func countersDecorator(ch <-chan string, padding int) mpb.DecoratorFunc {
	format := "%%%ds"
	var message string
	var current int64
	return func(s *mpb.Statistics, myWidth chan<- int, maxWidth <-chan int) string {
		if s.Total <= 0 {
			return fmt.Sprintf(fmt.Sprintf(format, padding), mpb.Format(s.Current).To(mpb.UnitBytes))
		}

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

		total := mpb.Format(s.Total).To(mpb.UnitBytes)
		completed := percentage(s.Total, s.Current, 100)
		counters := fmt.Sprintf("%.1f%% of %s", completed, total)
		myWidth <- utf8.RuneCountInString(counters)
		max := <-maxWidth
		return fmt.Sprintf(fmt.Sprintf(format, max+1), counters)
	}
}

func speedDecorator(failure <-chan struct{}) mpb.DecoratorFunc {
	var nowTime time.Time
	format := "%0.2f KiB/s"
	return func(s *mpb.Statistics, myWidth chan<- int, maxWidth <-chan int) string {
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
