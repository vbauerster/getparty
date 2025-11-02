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

func newRoundTripperBuilder(proxy string) (roundTripperBuilder, error) {
	b := roundTripperBuilder{cfg: new(config)}
	if proxy != "" {
		fixedURL, err := url.Parse(proxy)
		if err != nil {
			return b, BadProxyURL{err}
		}
		b.cfg.proxy = http.ProxyURL(fixedURL)
		b.history = append(b.history, func(log *log.Logger, prefix string) {
			log.Println(prefix, "proxy set to:", proxy)
		})
	}
	return b, nil
}

func (b roundTripperBuilder) tls(config *tls.Config) roundTripperBuilder {
	b.cfg.tls = config
	b.history = append(b.history, func(log *log.Logger, prefix string) {
		log.Println(prefix, "tls set to:", config)
	})
	return b
}

func (b roundTripperBuilder) pool(ok bool) roundTripperBuilder {
	b.cfg.pooled = ok
	b.history = append(b.history, func(log *log.Logger, prefix string) {
		log.Println(prefix, "pool set to:", ok)
	})
	return b
}

func (b roundTripperBuilder) debug(log *log.Logger, prefix string) {
	for _, fn := range b.history {
		fn(log, prefix)
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
	b.cfg = nil
	b.history = nil
	return transport
}
