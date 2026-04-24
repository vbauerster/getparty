//go:build windows

package getparty

import "github.com/vbauerster/mpb/v8"

func distinctBarRefiller() mpb.BarFiller {
	return baseBarStyle.Refiller("$").Build()
}
