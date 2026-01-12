package getparty

import (
	"crypto/tls"
	"log"
	"net/http"
	"net/url"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
)

type config struct {
	tls    *tls.Config
	proxy  func(*http.Request) (*url.URL, error)
	pooled bool
}

type history struct {
	events []func(*log.Logger, string)
}

func (h *history) append(event func(*log.Logger, string)) {
	h.events = append(h.events, event)
}

func (h *history) debug(logger *log.Logger, prefix string) {
	for _, fn := range h.events {
		fn(logger, prefix)
	}
}

type roundTripperBuilder struct {
	cfg  *config
	hist *history
}

func newRoundTripperBuilder() roundTripperBuilder {
	return roundTripperBuilder{
		cfg:  new(config),
		hist: new(history),
	}
}

func (b roundTripperBuilder) tls(config *tls.Config) roundTripperBuilder {
	b.cfg.tls = config
	b.hist.append(func(logger *log.Logger, prefix string) {
		logger.Printf("%s: tls set to: %#v", prefix, config)
	})
	return b
}

func (b roundTripperBuilder) proxy(fixedURL *url.URL) roundTripperBuilder {
	if fixedURL == nil {
		b.cfg.proxy = nil
	} else {
		b.cfg.proxy = http.ProxyURL(fixedURL)
	}
	b.hist.append(func(logger *log.Logger, prefix string) {
		logger.Printf("%s: proxy set to: %#v", prefix, fixedURL)
	})
	return b
}

func (b roundTripperBuilder) pool(ok bool) roundTripperBuilder {
	b.cfg.pooled = ok
	b.hist.append(func(logger *log.Logger, prefix string) {
		logger.Printf("%s: pool set to: %t", prefix, ok)
	})
	return b
}

func (b roundTripperBuilder) debug(logger *log.Logger) {
	b.hist.debug(logger, "RT builder")
}

func (b roundTripperBuilder) build() http.RoundTripper {
	var transport *http.Transport
	if b.cfg.pooled {
		transport = cleanhttp.DefaultPooledTransport()
	} else {
		transport = cleanhttp.DefaultTransport()
	}
	if b.cfg.proxy != nil {
		transport.Proxy = b.cfg.proxy
	}
	if b.cfg.tls != nil {
		transport.TLSClientConfig = b.cfg.tls
	}
	b.hist.append(func(logger *log.Logger, prefix string) {
		logger.Printf("%s: build called", prefix)
	})
	return transport
}
