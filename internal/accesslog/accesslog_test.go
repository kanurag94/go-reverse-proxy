package accesslog_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/internal/accesslog"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func serve(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	accesslog.New(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func assertLogged(t *testing.T, log string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(log, w) {
			t.Errorf("log %q lacks %q", log, w)
		}
	}
}

func TestLogsRequest(t *testing.T) {
	log := captureLog(t)
	rec := serve(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "hello")
	}, "http://api.example.com/items?token=secret")

	if rec.Code != http.StatusCreated || rec.Body.String() != "hello" {
		t.Errorf("response changed: %d %q", rec.Code, rec.Body.String())
	}
	assertLogged(t, log.String(), "method=GET", "host=api.example.com", "path=/items ",
		"status=201", "bytes=5", "duration=", "aborted=false")
	if strings.Contains(log.String(), "secret") {
		t.Errorf("log leaked the query: %q", log)
	}
}

func TestLogsPathAsSent(t *testing.T) {
	log := captureLog(t)
	serve(func(w http.ResponseWriter, r *http.Request) {}, "/a%2Fb")
	assertLogged(t, log.String(), "path=/a%2Fb ")
}

func TestImplicitStatus(t *testing.T) {
	tests := []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"body only", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") }, "status=200 bytes=1"},
		{"nothing", func(w http.ResponseWriter, r *http.Request) {}, "status=200 bytes=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := captureLog(t)
			serve(tt.h, "/")
			assertLogged(t, log.String(), tt.want)
		})
	}
}

func TestKeepsFlushWorking(t *testing.T) {
	captureLog(t)
	var err error
	rec := serve(func(w http.ResponseWriter, r *http.Request) {
		err = http.NewResponseController(w).Flush()
	}, "/")

	if err != nil {
		t.Fatalf("Flush through the access log: %v", err)
	}
	if !rec.Flushed {
		t.Error("the underlying writer was not flushed")
	}
}

func TestLogsAbortedRequest(t *testing.T) {
	log := captureLog(t)
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("panic = %v, want ErrAbortHandler to propagate", v)
		}
		assertLogged(t, log.String(), "status=200", "bytes=7", "aborted=true")
	}()
	serve(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "partial")
		panic(http.ErrAbortHandler)
	}, "/")
}
