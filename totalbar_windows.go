//go:build windows

package getparty

import "github.com/vbauerster/mpb/v7"

func totalBarStyle() mpb.BarFillerBuilder {
	return mpb.BarStyle().Lbound(" ").Rbound(" ")
}
