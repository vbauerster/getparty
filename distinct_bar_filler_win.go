//go:build windows

package getparty

import "github.com/vbauerster/mpb/v8"

var barBuilder mpb.BarFillerBuilder = mpb.BarStyle().Lbound(" ").Rbound(" ").Filler("#").Tip("").Refiller("$")
