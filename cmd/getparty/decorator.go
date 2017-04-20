// Copyright (C) 2016-2017 Vladimir Bauer
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"math"
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

func speedDecorator() mpb.DecoratorFunc {
	var nowTime time.Time
	var prevSpd float64
	return func(s *mpb.Statistics, myWidth chan<- int, maxWidth <-chan int) string {
		if !s.Completed {
			nowTime = time.Now()
		}
		totTime := nowTime.Sub(s.StartTime)
		spd := float64(s.Current/1024) / totTime.Seconds()
		if math.Abs(prevSpd-spd) < 1 {
			spd = prevSpd // discard low delta spd
		}
		fmtSpd := fmt.Sprintf("%0.2fKiB/s", spd)
		prevSpd = spd
		myWidth <- utf8.RuneCountInString(fmtSpd)
		max := <-maxWidth
		return fmt.Sprintf(fmt.Sprintf("%%%ds ", max), fmtSpd)
	}
}

func etaDecorator(failure <-chan struct{}) mpb.DecoratorFunc {
	format := "ETA %02d:%02d"
	return func(s *mpb.Statistics, myWidth chan<- int, maxWidth <-chan int) string {
		select {
		case <-failure:
			eta := fmt.Sprintf(format, 0, 0)
			return fmt.Sprint(strings.Replace(eta, "0", "-", -1))
		default:
		}

		eta := s.Eta()
		hours := int64((eta / time.Hour) % 60)
		minutes := int64((eta / time.Minute) % 60)
		seconds := int64((eta / time.Second) % 60)

		var fmtEta string
		if hours > 0 {
			fmtEta = fmt.Sprintf(format+":%02d", hours, minutes, seconds)
		} else {
			fmtEta = fmt.Sprintf(format, minutes, seconds)
		}

		myWidth <- utf8.RuneCountInString(fmtEta)
		max := <-maxWidth

		return fmt.Sprintf(fmt.Sprintf("%%-%ds", max), fmtEta)
	}
}

func percentage(total, current int64, ratio int) float64 {
	if total == 0 || current > total {
		return 0
	}
	return float64(ratio) * float64(current) / float64(total)
}
