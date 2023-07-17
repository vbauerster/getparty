//go:build !windows

package getparty

import "github.com/vbauerster/mpb/v8"

func totalBarStyle() mpb.BarFillerBuilder {
	green := func(s string) string {
		return "\x1b[32m" + s + "\x1b[0m"
	}
	cyan := func(s string) string {
		return "\x1b[36m" + s + "\x1b[0m"
	}
	return mpb.BarStyle().Lbound(" ").Rbound(" ").RefillerMeta(green).FillerMeta(cyan).TipMeta(cyan)
}
