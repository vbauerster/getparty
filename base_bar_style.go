package getparty

import "github.com/vbauerster/mpb/v8"

func baseBarStyle() mpb.BarStyleComposer {
	return mpb.BarStyle().Lbound(" ").Rbound(" ").Refiller("#").Filler("#").Tip("")
}
