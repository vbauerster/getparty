package getparty

import (
	"crypto/tls"
	"net/http"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
)

type roundTripperBuilder struct {
	pooled    bool
	tlsConfig *tls.Config
}

func newRoundTripperBuilder(config *tls.Config) *roundTripperBuilder {
	return &roundTripperBuilder{
		tlsConfig: config,
	}
}

func (b *roundTripperBuilder) pool(ok bool) *roundTripperBuilder {
	b.pooled = ok
	return b
}

func (b *roundTripperBuilder) build() http.RoundTripper {
	var transport *http.Transport
	if b.pooled {
		transport = cleanhttp.DefaultPooledTransport()
	} else {
		transport = cleanhttp.DefaultTransport()
	}
	transport.TLSClientConfig = b.tlsConfig
	return transport
}
