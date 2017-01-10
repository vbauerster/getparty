package main

import (
	"fmt"
	"strconv"

	"github.com/vbauerster/mpb"
)

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

		total := mpb.Format(s.Total).To(mpb.UnitBytes)
		completed := percentage(s.Total, s.Current, 100)
		counters := fmt.Sprintf("%.1f%% of %s", completed, total)
		return fmt.Sprintf(layout, counters)
	}
}

func displayErr(err error, bar *mpb.Bar, ch chan<- string, padding int) {
	message := err.Error()
	if len(message) > padding-2 {
		bar.TrimLeftSpace()
		message = message[:padding-2] + "..."
	}
	ch <- message
}

func percentage(total, current int64, ratio int) float64 {
	return float64(ratio) * float64(current) / float64(total)
}
