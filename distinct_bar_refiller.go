//go:build !windows

package getparty

import "github.com/vbauerster/mpb/v8"

func distinctBarRefiller(style mpb.BarStyleComposer) mpb.BarFillerBuilder {
	green := func(s string) string {
		return "\x1b[32m" + s + "\x1b[0m"
	}
	return style.RefillerMeta(green)
}
