//go:build windows

package getparty

import "github.com/vbauerster/mpb/v8"

func distinctBarFiller() mpb.BarFiller {
	return baseBarStyle.Refiller("$").Build()
}
