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

type roundTripperBuilder struct {
	logger *log.Logger
	cfg    *config
}

func newRoundTripperBuilder(logger *log.Logger) roundTripperBuilder {
	return roundTripperBuilder{
		logger: logger,
		cfg:    new(config),
	}
}

func (b roundTripperBuilder) tls(config *tls.Config) roundTripperBuilder {
	b.cfg.tls = config
	b.logger.Printf("RT Builder: tls set to %#v", config)
	return b
}

func (b roundTripperBuilder) proxy(fixedURL *url.URL) roundTripperBuilder {
	if fixedURL == nil {
		b.cfg.proxy = nil
	} else {
		b.cfg.proxy = http.ProxyURL(fixedURL)
	}
	b.logger.Printf("RT Builder: proxy set to %#v", fixedURL)
	return b
}

func (b roundTripperBuilder) pool(ok bool) roundTripperBuilder {
	b.cfg.pooled = ok
	b.logger.Printf("RT Builder: pool set to %t", ok)
	return b
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
	b.logger.Printf("RT Builder: build called")
	return transport
}
