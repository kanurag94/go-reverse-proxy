package cache

import (
	"bytes"
	"net/http"
	"strconv"
	"time"
)

// capture passes a response through to the client and keeps a copy of it
// for the store, for as long as the response can still be stored.
type capture struct {
	http.ResponseWriter
	cacheStatus string
	freshness   func(code int, h http.Header) time.Duration // nil: never store
	onGiveUp    func()                                      // called once the response cannot be stored
	limit       int64

	code   int
	header http.Header   // the response headers before ours were added
	fresh  time.Duration // 0 once the response cannot be stored
	body   bytes.Buffer
}

func (c *capture) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
		c.header = c.Header().Clone()
		if c.freshness != nil {
			c.fresh = c.freshness(code, c.header)
		}
		if c.fresh <= 0 {
			c.giveUp()
		}
		c.Header().Add("Cache-Status", c.cacheStatus)
	}
	c.ResponseWriter.WriteHeader(code)
}

// implicitHeader sends the 200 that net/http would, if WriteHeader has not been called.
func (c *capture) implicitHeader() {
	if c.code == 0 {
		c.WriteHeader(http.StatusOK)
	}
}

func (c *capture) Write(b []byte) (int, error) {
	c.implicitHeader()
	n, err := c.ResponseWriter.Write(b)
	if c.fresh > 0 {
		if err != nil || int64(c.body.Len()+n) > c.limit {
			c.giveUp() // the client did not get it all, or it is too big
		} else {
			c.body.Write(b[:n])
		}
	}
	return n, err
}

// giveUp drops the copy for good and tells anyone waiting on this response.
func (c *capture) giveUp() {
	c.fresh, c.body = 0, bytes.Buffer{}
	if c.onGiveUp != nil {
		c.onGiveUp()
	}
}

// FlushError makes a flush before the first write still pass through WriteHeader.
func (c *capture) FlushError() error {
	c.implicitHeader()
	return http.NewResponseController(c.ResponseWriter).Flush()
}

func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// entry returns the captured response ready to store, or nil if it must not be.
// req is the request it answered, whose Vary values the entry keeps.
func (c *capture) entry(now time.Time, req http.Header) *entry {
	if c.fresh <= 0 || !lengthMatches(c.header, c.body.Len()) {
		return nil
	}
	return &entry{
		status: c.code, header: c.header, body: c.body.Bytes(),
		storedAt: now, freshFor: c.fresh, initialAge: initialAge(c.header),
		vary: varyValues(varyNames(c.header), req),
	}
}

func lengthMatches(h http.Header, n int) bool {
	cl := h.Get("Content-Length")
	return cl == "" || cl == strconv.Itoa(n)
}
