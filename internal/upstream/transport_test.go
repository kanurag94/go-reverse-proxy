package upstream_test

import (
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

func TestNewTransportDefaults(t *testing.T) {
	tr, err := upstream.NewTransport(upstream.Timeouts{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil {
		t.Error("Proxy is set: upstream traffic must not follow $HTTP_PROXY")
	}
	if !tr.DisableCompression {
		t.Error("compression enabled: Transport would add Accept-Encoding: gzip for clients that never asked")
	}
	if tr.MaxIdleConnsPerHost != 100 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 100 (Go's default of 2 churns connections under load)", tr.MaxIdleConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want the 30s default", tr.ResponseHeaderTimeout)
	}
	if tr.ExpectContinueTimeout != time.Second || !tr.ForceAttemptHTTP2 {
		t.Errorf("ExpectContinueTimeout %v, ForceAttemptHTTP2 %v: lost DefaultTransport's settings", tr.ExpectContinueTimeout, tr.ForceAttemptHTTP2)
	}
}

func TestNewTransportUsesTimeouts(t *testing.T) {
	tr, err := upstream.NewTransport(upstream.Timeouts{Connect: time.Second, ResponseHeader: 10 * time.Minute}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tr.ResponseHeaderTimeout != 10*time.Minute {
		t.Errorf("ResponseHeaderTimeout = %v, want 10m", tr.ResponseHeaderTimeout)
	}
}
