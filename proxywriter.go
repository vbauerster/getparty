package getparty

import (
	"github.com/vbauerster/mpb/v7"
)

type proxyWriter struct {
	bar *mpb.Bar
}

func (x proxyWriter) Write(p []byte) (int, error) {
	x.bar.IncrBy(len(p))
	return len(p), nil
}
