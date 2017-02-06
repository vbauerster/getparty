package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
	layout := "%" + strconv.Itoa(padding) + "s"
	var message string
	var current int64
	return func(s *mpb.Statistics) string {
		select {
		case message = <-ch:
			current = s.Current
		default:
		}

		if message != "" && current == s.Current {
			return fmt.Sprintf(layout, message)
		}

		if s.Total <= 0 {
			return fmt.Sprintf("%10s", mpb.Format(s.Current).To(mpb.UnitBytes))
		}

		total := mpb.Format(s.Total).To(mpb.UnitBytes)
		completed := percentage(s.Total, s.Current, 100)
		counters := fmt.Sprintf("%.1f%% of %s", completed, total)
		return fmt.Sprintf(layout, counters)
	}
}

func etaDecorator(failure <-chan struct{}) mpb.DecoratorFunc {
	format := "ETA %02d:%02d:%02d"
	return func(s *mpb.Statistics) string {
		select {
		case <-failure:
			etaLen := len(fmt.Sprintf(format, 0, 0, 0))
			return "--" + strings.Repeat(" ", etaLen-2)
		default:
		}

		eta := s.Eta()
		hours := int64((eta / time.Hour) % 60)
		minutes := int64((eta / time.Minute) % 60)
		seconds := int64((eta / time.Second) % 60)

		return fmt.Sprintf(format, hours, minutes, seconds)
	}
}

func percentage(total, current int64, ratio int) float64 {
	return float64(ratio) * float64(current) / float64(total)
}
