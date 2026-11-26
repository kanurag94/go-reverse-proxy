package hook_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/hook"
)

var client = &http.Client{Timeout: 5 * time.Second}

// serve runs next behind the hooks on a real server, so flushing and trailers are real.
func serve(t *testing.T, next http.HandlerFunc, hooks ...hook.Hook) string {
	t.Helper()
	srv := httptest.NewServer(hook.New(next, hooks...))
	t.Cleanup(srv.Close)
	return srv.URL
}

func read(t *testing.T, res *http.Response, err error) (*http.Response, string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(b)
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := client.Get(url)
	return read(t, res, err)
}

func post(t *testing.T, url string, body io.Reader) (*http.Response, string) {
	t.Helper()
	res, err := client.Post(url, "text/plain", body)
	return read(t, res, err)
}

// always processes every request with p.
func always(p hook.Processor) hook.Hook {
	return func(*http.Request) *hook.Processor { return &p }
}

// text answers "x" with trailers announced and set the way proxy.Handler does.
func text(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Trailer", "Grpc-Status, X-Debug")
	io.WriteString(w, "x")
	w.Header()[http.TrailerPrefix+"Grpc-Status"] = []string{"0"}
	w.Header()[http.TrailerPrefix+"X-Debug"] = []string{"1"}
}

// echo answers with the request's X-Trace values, Content-Length and body.
func echo(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	fmt.Fprintf(w, "%s|%d|%s", strings.Join(r.Header.Values("X-Trace"), ","), r.ContentLength, b)
}

// suffix marks the response headers and each chunk with its name.
func suffix(name string) hook.Hook {
	return always(hook.Processor{
		OnResponseHeaders: func(status int, h http.Header) *hook.ImmediateResponse {
			h.Add("X-Trace", name)
			return nil
		},
		OnResponseBody: func(chunk []byte, endOfStream bool) []byte { return []byte(string(chunk) + name) },
	})
}

func TestRequestHooksRunInOrder(t *testing.T) {
	mark := func(name string) hook.Hook {
		return always(hook.Processor{OnRequestHeaders: func(h http.Header) *hook.ImmediateResponse {
			h.Add("X-Trace", name)
			return nil
		}})
	}
	if _, body := post(t, serve(t, echo, mark("a"), mark("b")), strings.NewReader("hi")); body != "a,b|2|hi" {
		t.Errorf("body = %q, want a,b|2|hi", body)
	}
}

func TestRequestHeadersImmediateResponse(t *testing.T) {
	var reached atomic.Bool
	upstream := func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }
	limit := always(hook.Processor{OnRequestHeaders: func(http.Header) *hook.ImmediateResponse {
		return &hook.ImmediateResponse{Status: http.StatusTooManyRequests, Body: []byte("over budget")}
	}})
	res, body := post(t, serve(t, upstream, limit), strings.NewReader("hi"))
	if res.StatusCode != http.StatusTooManyRequests || body != "over budget" || reached.Load() {
		t.Errorf("got %d %q, upstream reached %v; want 429 over budget, not reached", res.StatusCode, body, reached.Load())
	}
}

func TestRequestBodyIsReplaced(t *testing.T) {
	upper := always(hook.Processor{OnRequestBody: func(b []byte) ([]byte, *hook.ImmediateResponse) {
		return bytes.ToUpper(append(b, "!"...)), nil
	}})
	if _, body := post(t, serve(t, echo, upper), strings.NewReader("hi")); body != "|3|HI!" {
		t.Errorf("body = %q, want the new body with its own length: |3|HI!", body)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	keep := always(hook.Processor{OnRequestBody: func(b []byte) ([]byte, *hook.ImmediateResponse) { return b, nil }})
	big := strings.NewReader(strings.Repeat("x", hook.MaxBufferedBody+1))
	if res, _ := post(t, serve(t, echo, keep), big); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", res.StatusCode)
	}
}

func TestRequestBodyStreamsWithoutBodyHook(t *testing.T) {
	mark := always(hook.Processor{OnRequestHeaders: func(http.Header) *hook.ImmediateResponse { return nil }})
	pr, pw := io.Pipe()
	go func() { io.WriteString(pw, "hi"); pw.Close() }()
	if _, body := post(t, serve(t, echo, mark), pr); body != "|-1|hi" {
		t.Errorf("body = %q, want it streamed with unknown length: |-1|hi", body)
	}
}

func TestRequestTrailersAreSeen(t *testing.T) {
	var got atomic.Value
	see := always(hook.Processor{OnRequestTrailers: func(tr http.Header) *hook.ImmediateResponse {
		got.Store(tr.Get("X-Checksum"))
		return nil
	}})
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, serve(t, echo, see), pr)
	req.Trailer = http.Header{"X-Checksum": nil}
	go func() { io.WriteString(pw, "hi"); req.Trailer.Set("X-Checksum", "abc"); pw.Close() }()
	res, err := client.Do(req)
	read(t, res, err)
	if got.Load() != "abc" {
		t.Errorf("trailer = %v, want abc", got.Load())
	}
}

func TestResponseHooksRunInReverseOrder(t *testing.T) {
	res, body := get(t, serve(t, text, suffix("a"), suffix("b")))
	if body != "xbaba" || !slices.Equal(res.Header.Values("X-Trace"), []string{"b", "a"}) {
		t.Errorf("body %q, trace %q; want xbaba (chunk then end), b then a", body, res.Header.Values("X-Trace"))
	}
}

func TestResponseHeadersImmediateResponse(t *testing.T) {
	deny := always(hook.Processor{OnResponseHeaders: func(int, http.Header) *hook.ImmediateResponse {
		return &hook.ImmediateResponse{Status: http.StatusForbidden, Body: []byte("denied")}
	}})
	res, body := get(t, serve(t, text, deny))
	if res.StatusCode != http.StatusForbidden || body != "denied" {
		t.Errorf("got %d %q, want 403 denied", res.StatusCode, body)
	}
}

func TestResponseBodyEditDropsContentLength(t *testing.T) {
	sized := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1") // as proxy.Handler copies it from the upstream
		io.WriteString(w, "x")
	}
	bang := always(hook.Processor{OnResponseBody: func(chunk []byte, endOfStream bool) []byte {
		if endOfStream {
			return []byte("!")
		}
		return chunk
	}})
	if _, body := get(t, serve(t, sized, bang)); body != "x!" {
		t.Errorf("body = %q, want x! despite the upstream's Content-Length", body)
	}
}

func TestResponseTrailersAreEdited(t *testing.T) {
	edit := always(hook.Processor{OnResponseTrailers: func(tr http.Header) {
		tr.Set("Grpc-Status", "13")
		tr.Del("X-Debug")
		tr.Set("X-Hook", "yes")
	}})
	res, _ := get(t, serve(t, text, edit))
	if res.Trailer.Get("Grpc-Status") != "13" || res.Trailer.Get("X-Hook") != "yes" || res.Trailer.Get("X-Debug") != "" {
		t.Errorf("trailers = %v, want Grpc-Status 13, X-Hook yes, no X-Debug", res.Trailer)
	}
}

func TestResponsePhasesRunInOrderAndEndOnce(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	note := func(s string) { mu.Lock(); seen = append(seen, s); mu.Unlock() }
	record := always(hook.Processor{
		OnResponseHeaders: func(int, http.Header) *hook.ImmediateResponse { note("headers"); return nil },
		OnResponseBody: func(chunk []byte, endOfStream bool) []byte {
			if endOfStream {
				note("end")
				return []byte("!")
			}
			note("body:" + string(chunk))
			return chunk
		},
		OnResponseTrailers: func(http.Header) { note("trailers") },
	})
	_, body := get(t, serve(t, text, record))
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"headers", "body:x", "end", "trailers"}; body != "x!" || !slices.Equal(seen, want) {
		t.Errorf("body %q, phases %q; want x! and %q", body, seen, want)
	}
}

func TestNilProcessorSkipsRequest(t *testing.T) {
	skip := func(*http.Request) *hook.Processor { return nil }
	if _, body := get(t, serve(t, text, skip)); body != "x" {
		t.Errorf("body = %q, want x untouched", body)
	}
}

func TestResponseChunksAreNotHeldBack(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	next := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first")
		http.NewResponseController(w).Flush()
		<-release
	}
	pass := always(hook.Processor{OnResponseBody: func(c []byte, endOfStream bool) []byte { return c }})
	res, err := client.Get(serve(t, next, pass))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "first" {
		t.Errorf("read %q, %v; want first while the upstream still holds the rest", buf, err)
	}
}
