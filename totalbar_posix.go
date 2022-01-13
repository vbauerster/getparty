//go:build !windows

package getparty

import "github.com/vbauerster/mpb/v7"

func totalBarStyle() mpb.BarFillerBuilder {
	return mpb.BarStyle().Lbound(" \x1b[36m").Padding("\x1b[0m-").Rbound("\x1b[0m ")
}
