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
	cfg     *config
	history []func(*log.Logger, string)
}

func newRoundTripperBuilder() roundTripperBuilder {
	return roundTripperBuilder{cfg: new(config)}
}

func (b roundTripperBuilder) tls(config *tls.Config) roundTripperBuilder {
	b.cfg.tls = config
	b.history = append(b.history, func(logger *log.Logger, prefix string) {
		logger.Println(prefix, "tls set to:", config)
	})
	return b
}

func (b roundTripperBuilder) proxy(fixedURL *url.URL) roundTripperBuilder {
	if fixedURL != nil {
		b.cfg.proxy = http.ProxyURL(fixedURL)
		b.history = append(b.history, func(logger *log.Logger, prefix string) {
			logger.Println(prefix, "proxy set to:", fixedURL.String())
		})
	}
	return b
}

func (b roundTripperBuilder) pool(ok bool) roundTripperBuilder {
	b.cfg.pooled = ok
	b.history = append(b.history, func(logger *log.Logger, prefix string) {
		logger.Println(prefix, "pool set to:", ok)
	})
	return b
}

func (b roundTripperBuilder) debug(logger *log.Logger, prefix string) {
	for _, fn := range b.history {
		fn(logger, prefix)
	}
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
	return transport
}
