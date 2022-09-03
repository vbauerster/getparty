//go:build windows

package getparty

import "github.com/vbauerster/mpb/v8"

func totalBarStyle() mpb.BarFillerBuilder {
	return mpb.BarStyle().Lbound(" ").Rbound(" ")
}
