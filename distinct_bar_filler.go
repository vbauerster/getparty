//go:build !windows

package getparty

import "github.com/vbauerster/mpb/v8"

func green(s string) string {
	return "\x1b[32m" + s + "\x1b[0m"
}

var barBuilder mpb.BarFillerBuilder = mpb.BarStyle().Lbound(" ").Rbound(" ").Filler("#").Tip("").Refiller("#").RefillerMeta(green)
