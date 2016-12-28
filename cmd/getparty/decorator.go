package main

import (
	"fmt"
	"strconv"

	"github.com/vbauerster/mpb"
)

func countersDecorator(padding int) mpb.DecoratorFunc {
	layout := "%" + strconv.Itoa(padding) + "s"
	return func(s *mpb.Statistics) string {
		total := mpb.Format(s.Total).To(mpb.UnitBytes)
		completed := percentage(s.Total, s.Current, 100)
		str := fmt.Sprintf("%.1f%% of %s", completed, total)
		return fmt.Sprintf(layout, str)
	}
}

func percentage(total, current int64, ratio int) float64 {
	return float64(ratio) * float64(current) / float64(total)
}
