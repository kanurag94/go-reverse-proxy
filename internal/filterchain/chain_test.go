package filterchain_test

import (
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

	"github.com/kanurag94/go-reverse-proxy/internal/filterchain"
	"github.com/kanurag94/go-reverse-proxy/pkg/hook"
)

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, s)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// recorder logs every callback it receives.
type recorder struct {
	hook.EmptyFilter
	tag string
	log *callLog
}

func (r *recorder) OnRequestHeaders(h http.Header, eos bool) hook.Status {
	r.log.add(r.tag + " req-headers")
	return hook.Continue
}

func (r *recorder) OnRequestBody(b []byte, eos bool) []byte {
	r.log.add(fmt.Sprintf("%s req-body %q end=%v", r.tag, b, eos))
	return b
}

func (r *recorder) OnResponseHeaders(status int, h http.Header, eos bool) hook.Status {
	r.log.add(fmt.Sprintf("%s resp-headers %d", r.tag, status))
	return hook.Continue
}

func (r *recorder) OnResponseBody(b []byte, eos bool) []byte {
	r.log.add(fmt.Sprintf("%s resp-body %q end=%v", r.tag, b, eos))
	return b
}

func (r *recorder) OnStreamComplete() { r.log.add(r.tag + " complete") }

type factory struct {
	make    func(h hook.Handle) hook.Filter
	created atomic.Int32
}

func (f *factory) Create(h hook.Handle) hook.Filter {
	f.created.Add(1)
	return f.make(h)
}

func (f *factory) OnDestroy() {}

func recording(tag string, log *callLog) *factory {
	return &factory{make: func(hook.Handle) hook.Filter { return &recorder{tag: tag, log: log} }}
}

type headerEditor struct{ hook.EmptyFilter }

func (headerEditor) OnRequestHeaders(h http.Header, eos bool) hook.Status {
	h.Set("X-Req", "added")
	return hook.Continue
}

func (headerEditor) OnResponseHeaders(status int, h http.Header, eos bool) hook.Status {
	h.Set("X-Resp", "added")
	return hook.Continue
}

type denier struct {
	hook.EmptyFilter
	h   hook.Handle
	log *callLog
}

func (d *denier) OnRequestHeaders(h http.Header, eos bool) hook.Status {
	d.h.SendLocalReply(http.StatusForbidden, nil, []byte("denied"))
	return hook.Continue
}

func (d *denier) OnStreamComplete() { d.log.add("deny complete") }

var client = &http.Client{Timeout: 5 * time.Second}

func serve(t *testing.T, next http.HandlerFunc, factories ...hook.FilterFactory) string {
	t.Helper()
	srv := httptest.NewServer(filterchain.New(next, factories...))
	t.Cleanup(srv.Close)
	return srv.URL
}

func ok(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }

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

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	res, err := client.Post(url, "text/plain", strings.NewReader(body))
	return read(t, res, err)
}

func TestFiltersRunInOrderAndResponsesInReverse(t *testing.T) {
	log := &callLog{}
	echo := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	}
	post(t, serve(t, echo, recording("a", log), recording("b", log)), "hi")
	want := []string{
		"a req-headers", "b req-headers",
		`a req-body "hi" end=false`, `b req-body "hi" end=false`, `a req-body "" end=true`, `b req-body "" end=true`,
		"b resp-headers 200", "a resp-headers 200",
		`b resp-body "ok" end=false`, `a resp-body "ok" end=false`, `b resp-body "" end=true`, `a resp-body "" end=true`,
		"b complete", "a complete",
	}
	if got := log.all(); !slices.Equal(got, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestNewFilterPerRequest(t *testing.T) {
	log := &callLog{}
	f := &factory{make: func(h hook.Handle) hook.Filter {
		return &recorder{tag: h.Request().URL.Path, log: log}
	}}
	url := serve(t, ok, f)
	get(t, url+"/one")
	get(t, url+"/two")
	if f.created.Load() != 2 {
		t.Errorf("created %d filters, want one per request", f.created.Load())
	}
	for _, want := range []string{"/one complete", "/two complete"} {
		if !slices.Contains(log.all(), want) {
			t.Errorf("calls %q lack %q: each request needs its own filter", log.all(), want)
		}
	}
}

func TestFiltersEditHeadersInPlace(t *testing.T) {
	f := &factory{make: func(hook.Handle) hook.Filter { return headerEditor{} }}
	upstream := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Header.Get("X-Req")) }
	res, body := get(t, serve(t, upstream, f)+"/")
	if body != "added" || res.Header.Get("X-Resp") != "added" {
		t.Errorf("upstream saw X-Req %q, client saw X-Resp %q; want both added", body, res.Header.Get("X-Resp"))
	}
}

func TestLocalReplySkipsUpstreamAndLaterFilters(t *testing.T) {
	log := &callLog{}
	var reached atomic.Bool
	upstream := func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }
	deny := &factory{make: func(h hook.Handle) hook.Filter { return &denier{h: h, log: log} }}
	res, body := get(t, serve(t, upstream, deny, recording("later", log))+"/")
	if res.StatusCode != http.StatusForbidden || body != "denied" || reached.Load() {
		t.Errorf("got %d %q, upstream reached %v; want 403 denied, not reached", res.StatusCode, body, reached.Load())
	}
	if got := log.all(); !slices.Equal(got, []string{"later complete", "deny complete"}) {
		t.Errorf("calls %q, want only the two completions", got)
	}
}

func TestResponseChunksAreNotHeldBack(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	upstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first")
		http.NewResponseController(w).Flush()
		<-release
	}
	res, err := client.Get(serve(t, upstream, recording("a", &callLog{})) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "first" {
		t.Errorf("read %q, %v while the upstream holds the rest; want first", buf, err)
	}
}

func TestStreamCompleteRunsWhenTheUpstreamAborts(t *testing.T) {
	log := &callLog{}
	upstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "part")
		panic(http.ErrAbortHandler)
	}
	if res, err := client.Get(serve(t, upstream, recording("a", log)) + "/"); err == nil {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	if !slices.Contains(log.all(), "a complete") {
		t.Errorf("calls %q; want OnStreamComplete even for an aborted response", log.all())
	}
}

func TestNilFilterSkipsTheRequest(t *testing.T) {
	skip := &factory{make: func(hook.Handle) hook.Filter { return nil }}
	if _, body := get(t, serve(t, ok, skip)+"/"); body != "ok" {
		t.Errorf("body %q, want ok untouched", body)
	}
}

// prefixer prepends its tag to request and response bodies.
type prefixer struct {
	hook.EmptyFilter
	tag        string
	dropLength bool
	started    [2]bool
}

func (p *prefixer) OnRequestHeaders(h http.Header, eos bool) hook.Status {
	if p.dropLength {
		h.Del("Content-Length")
	}
	return hook.Continue
}

func (p *prefixer) OnResponseHeaders(status int, h http.Header, eos bool) hook.Status {
	if p.dropLength {
		h.Del("Content-Length")
	}
	return hook.Continue
}

func (p *prefixer) prefix(i int, b []byte) []byte {
	if p.started[i] || len(b) == 0 {
		return b
	}
	p.started[i] = true
	return append([]byte(p.tag+":"), b...)
}

func (p *prefixer) OnRequestBody(b []byte, eos bool) []byte  { return p.prefix(0, b) }
func (p *prefixer) OnResponseBody(b []byte, eos bool) []byte { return p.prefix(1, b) }

func prefixing(tag string, dropLength bool) *factory {
	return &factory{make: func(hook.Handle) hook.Filter { return &prefixer{tag: tag, dropLength: dropLength} }}
}

func TestBodyEditsHappenInSequence(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }
	_, body := post(t, serve(t, echo, prefixing("a", true), prefixing("b", true)), "hi")
	if body != "a:b:b:a:hi" {
		t.Errorf("body %q, want a:b:b:a:hi", body)
	}
}

func TestStaleContentLengthAbortsTheResponse(t *testing.T) {
	sized := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		io.WriteString(w, "ok")
	}
	res, err := client.Get(serve(t, sized, prefixing("a", false)) + "/")
	if err == nil {
		b, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr == nil {
			t.Errorf("got a complete-looking body %q; want the response aborted", b)
		}
	}
}

type holder struct {
	hook.EmptyFilter
	held []byte
}

func (h *holder) OnResponseBody(b []byte, eos bool) []byte {
	h.held = append(h.held, b...)
	if !eos {
		return nil
	}
	return h.held
}

func TestHoldingBackReleasesAtTheEnd(t *testing.T) {
	chunks := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "a")
		http.NewResponseController(w).Flush()
		io.WriteString(w, "b")
	}
	hold := &factory{make: func(hook.Handle) hook.Filter { return &holder{} }}
	if _, body := get(t, serve(t, chunks, hold)+"/"); body != "ab" {
		t.Errorf("body %q, want ab released at the end", body)
	}
}

func TestDroppedRequestLengthIsSentChunked(t *testing.T) {
	length := func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.ContentLength) }
	if _, body := post(t, serve(t, length, prefixing("a", true)), "hi"); body != "a:-1" {
		t.Errorf("got %s, want a:-1: the upstream must see an unknown length", body)
	}
}

type buffering struct {
	hook.EmptyFilter
	h       hook.Handle
	request bool
	inspect func(h hook.Handle, body []byte) []byte
	calls   *callLog
}

func (b *buffering) OnRequestHeaders(http.Header, bool) hook.Status {
	if b.request {
		return hook.StopAndBuffer
	}
	return hook.Continue
}

func (b *buffering) OnRequestBody(body []byte, eos bool) []byte {
	if !b.request {
		return body
	}
	b.calls.add(fmt.Sprintf("req-body %q end=%v", body, eos))
	return b.inspect(b.h, body)
}

func (b *buffering) OnResponseHeaders(int, http.Header, bool) hook.Status {
	if b.request {
		return hook.Continue
	}
	return hook.StopAndBuffer
}

func (b *buffering) OnResponseBody(body []byte, eos bool) []byte {
	if b.request {
		return body
	}
	b.calls.add(fmt.Sprintf("resp-body %q end=%v", body, eos))
	return b.inspect(b.h, body)
}

func bufferingOn(request bool, log *callLog, inspect func(hook.Handle, []byte) []byte) *factory {
	return &factory{make: func(h hook.Handle) hook.Filter {
		return &buffering{h: h, request: request, inspect: inspect, calls: log}
	}}
}

func keep(_ hook.Handle, b []byte) []byte { return b }

func TestBufferedRequestBodyArrivesWhole(t *testing.T) {
	log := &callLog{}
	echo := func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }
	pr, pw := io.Pipe()
	go func() {
		io.WriteString(pw, "hello ")
		io.WriteString(pw, "world")
		pw.Close()
	}()
	res, err := client.Post(serve(t, echo, bufferingOn(true, log, keep)), "text/plain", pr)
	if _, body := read(t, res, err); body != "hello world" {
		t.Errorf("upstream got %q, want hello world", body)
	}
	if got := log.all(); !slices.Equal(got, []string{`req-body "hello world" end=true`}) {
		t.Errorf("calls %q, want one call with the whole body", got)
	}
}

func TestBufferedRequestCanBeDenied(t *testing.T) {
	var reached atomic.Bool
	upstream := func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }
	deny := func(h hook.Handle, b []byte) []byte {
		if strings.Contains(string(b), "secret") {
			h.SendLocalReply(http.StatusForbidden, nil, []byte("denied"))
		}
		return b
	}
	res, body := post(t, serve(t, upstream, bufferingOn(true, &callLog{}, deny)), "my secret")
	if res.StatusCode != http.StatusForbidden || body != "denied" || reached.Load() {
		t.Errorf("got %d %q, upstream reached %v; want 403 denied, not reached", res.StatusCode, body, reached.Load())
	}
}

func TestBufferedRequestRewriteSetsLength(t *testing.T) {
	seen := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%d %s", r.ContentLength, b)
	}
	rewrite := func(hook.Handle, []byte) []byte { return []byte("rewritten") }
	if _, body := post(t, serve(t, seen, bufferingOn(true, &callLog{}, rewrite)), "x"); body != "9 rewritten" {
		t.Errorf("upstream saw %q, want 9 rewritten", body)
	}
}

func TestBufferedRequestTooLarge(t *testing.T) {
	big := strings.Repeat("x", filterchain.MaxBufferedBody+1)
	res, _ := post(t, serve(t, ok, bufferingOn(true, &callLog{}, keep)), big)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", res.StatusCode)
	}
}

func TestBufferedResponseCanBeReplaced(t *testing.T) {
	leaky := func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "a leak") }
	redact := func(h hook.Handle, b []byte) []byte {
		if strings.Contains(string(b), "leak") {
			h.SendLocalReply(http.StatusForbidden, nil, []byte("redacted"))
		}
		return b
	}
	res, body := get(t, serve(t, leaky, bufferingOn(false, &callLog{}, redact))+"/")
	if res.StatusCode != http.StatusForbidden || body != "redacted" {
		t.Errorf("got %d %q, want 403 redacted", res.StatusCode, body)
	}
}

func TestBufferedResponseArrivesWholeWithItsLength(t *testing.T) {
	log := &callLog{}
	chunks := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "a")
		http.NewResponseController(w).Flush()
		io.WriteString(w, "b")
	}
	double := func(_ hook.Handle, b []byte) []byte { return append(b, b...) }
	res, body := get(t, serve(t, chunks, bufferingOn(false, log, double))+"/")
	if body != "abab" || res.ContentLength != 4 {
		t.Errorf("got %q with length %d, want abab with length 4", body, res.ContentLength)
	}
	if got := log.all(); !slices.Equal(got, []string{`resp-body "ab" end=true`}) {
		t.Errorf("calls %q, want one call with the whole body", got)
	}
}

func TestBufferedResponseTooLarge(t *testing.T) {
	huge := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", filterchain.MaxBufferedBody+1))
	}
	res, _ := get(t, serve(t, huge, bufferingOn(false, &callLog{}, keep))+"/")
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", res.StatusCode)
	}
}

func TestBufferedResponseReplacesUpstreamLength(t *testing.T) {
	sized := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		io.WriteString(w, "ab")
	}
	double := func(_ hook.Handle, b []byte) []byte { return append(b, b...) }
	res, body := get(t, serve(t, sized, bufferingOn(false, &callLog{}, double))+"/")
	if body != "abab" || res.ContentLength != 4 {
		t.Errorf("got %q with length %d, want abab with length 4", body, res.ContentLength)
	}
}
