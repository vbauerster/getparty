//go:build !windows

package getparty

import "github.com/vbauerster/mpb/v8"

func green(s string) string {
	return "\x1b[32m" + s + "\x1b[0m"
}

func distinctBarRefiller() mpb.BarFiller {
	return baseBarStyle.Refiller("#").RefillerMeta(green).Build()
}
