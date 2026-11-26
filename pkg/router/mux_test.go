package router_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/pkg/router"
)

func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name)
	})
}

func testMux() *router.Mux {
	return router.NewMux([]router.Host{
		{
			Domains: []string{"api.example.com", "api.internal"},
			Routes: []router.Route{
				{Prefix: "/v1/chat", Handler: named("chat")},
				{Prefix: "/v1", Handler: named("v1")},
				{Prefix: "/", Handler: named("api-root")},
			},
		},
		{
			Domains: []string{"www.example.com"},
			Routes: []router.Route{
				{Prefix: "/static", Handler: named("static")},
				{Prefix: "/admin", Handler: named("admin")},
			},
		},
	})
}

func TestMuxRouting(t *testing.T) {
	tests := []struct {
		host, target string
		wantCode     int
		wantBody     string
	}{
		{"api.example.com", "/v1/chat/completions", 200, "chat"},
		{"api.example.com", "/v1/models", 200, "v1"},
		{"api.example.com", "/v1", 200, "v1"},
		{"api.example.com", "/v10", 200, "api-root"},
		{"api.example.com", "/", 200, "api-root"},
		{"api.internal", "/v1", 200, "v1"},
		{"www.example.com", "/static/app.js", 200, "static"},
		{"www.example.com", "/%61dmin/users", 200, "admin"},
		{"www.example.com", "/other", 404, ""},

		{"API.Example.COM", "/v1", 200, "v1"},
		{"api.example.com:8443", "/v1", 200, "v1"},
		{"api.example.com.", "/v1", 200, "v1"},
		{"API.example.com.:8443", "/v1", 200, "v1"},
		{"unknown.example.com", "/", 404, ""},
		{"", "/", 404, ""},

		{"api.example.com", "/v1/../admin", 400, ""},
		{"api.example.com", "/v1/%2e%2e/admin", 400, ""},
		{"api.example.com", "/v1%2F..%2Fadmin", 400, ""},
		{"api.example.com", "/v1/./models", 400, ""},
		{"api.example.com", "/v1/..", 400, ""},
		{"api.example.com", "/v1/..data", 200, "v1"},
		{"api.example.com", "/v1/.well-known", 200, "v1"},
		{"api.example.com", "/v1/..;/admin", 400, ""},
		{"api.example.com", "/v1/.;x/models", 400, ""},
		{"api.example.com", "/v1/a;b", 200, "v1"},
		{"api.example.com", "a:%2fadmin", 400, ""},
		{"api.example.com", "*", 400, ""},
	}
	mux := testMux()
	for _, tt := range tests {
		t.Run(tt.host+tt.target, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("routed to %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
