//go:build windows

package getparty

import "github.com/vbauerster/mpb/v8"

func distinctBarRefiller(style mpb.BarStyleComposer) mpb.BarFillerBuilder {
	return style.Refiller("$")
}
