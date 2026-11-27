package upstream_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/upstream"
)

func TestNewTransportDefaults(t *testing.T) {
	tr, err := upstream.NewTransport(upstream.Timeouts{}, "", false)
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
	tr, err := upstream.NewTransport(upstream.Timeouts{Connect: time.Second, ResponseHeader: 10 * time.Minute}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if tr.ResponseHeaderTimeout != 10*time.Minute {
		t.Errorf("ResponseHeaderTimeout = %v, want 10m", tr.ResponseHeaderTimeout)
	}
}

func protoServer(t *testing.T) string {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestH2CTransport(t *testing.T) {
	url := protoServer(t)
	for h2c, want := range map[bool]string{true: "HTTP/2.0", false: "HTTP/1.1"} {
		tr, err := upstream.NewTransport(upstream.Timeouts{}, "", h2c)
		if err != nil {
			t.Fatal(err)
		}
		res, err := (&http.Client{Transport: tr}).Get(url)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != want {
			t.Errorf("h2c=%v: upstream saw %s, want %s", h2c, b, want)
		}
	}
}
