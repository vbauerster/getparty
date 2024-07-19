package getparty

import (
	"crypto/tls"
	"net/http"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
)

type roundTripperBuilder struct {
	transport *http.Transport
}

func newRoundTripperBuilder(pooled bool) roundTripperBuilder {
	if pooled {
		return roundTripperBuilder{
			transport: cleanhttp.DefaultPooledTransport(),
		}
	}
	return roundTripperBuilder{
		transport: cleanhttp.DefaultTransport(),
	}
}

func (b roundTripperBuilder) withTLSConfig(config *tls.Config) roundTripperBuilder {
	b.transport.TLSClientConfig = config
	return b
}

func (b roundTripperBuilder) build() http.RoundTripper {
	return b.transport
}
