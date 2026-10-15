package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// Writers beneath chi, one per combination of the optional interfaces chi
// probes for.
type shapeFlush struct{ *httptest.ResponseRecorder }

type shapeFlushHijack struct{ shapeFlush }

func (shapeFlushHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

type shapeFlushHijackReaderFrom struct{ shapeFlushHijack }

func (s shapeFlushHijackReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(s.ResponseRecorder, r)
}

type shapeFlushPusher struct{ shapeFlush }

func (shapeFlushPusher) Push(string, *http.PushOptions) error { return nil }

type shapeHijackOnly struct{ http.ResponseWriter }

func (shapeHijackOnly) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

func optionalInterfaces(w http.ResponseWriter) [4]bool {
	_, fl := w.(http.Flusher)
	_, hj := w.(http.Hijacker)
	_, rf := w.(io.ReaderFrom)
	_, ps := w.(http.Pusher)
	return [4]bool{fl, hj, rf, ps}
}

// #2186 codex r1: the wrapper must not hide an interface the chi writer has
// (io.ReaderFrom, http.Pusher) nor add one it lacks (Flusher, Hijacker).
func TestBUG3827_WrapperKeepsTheChiWritersInterfaces(t *testing.T) {
	rec := func() *httptest.ResponseRecorder { return httptest.NewRecorder() }
	cases := []struct {
		name  string
		below http.ResponseWriter
		proto int
	}{
		{"h1 fancy", shapeFlushHijackReaderFrom{shapeFlushHijack{shapeFlush{rec()}}}, 1},
		{"h1 flush+hijack", shapeFlushHijack{shapeFlush{rec()}}, 1},
		{"h1 hijack only", shapeHijackOnly{rec()}, 1},
		{"h1 flush only", shapeFlush{rec()}, 1},
		{"h2 fancy", shapeFlushPusher{shapeFlush{rec()}}, 2},
		{"h2 flush only", shapeFlush{rec()}, 2},
		{"basic", shapeHijackOnly{rec()}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chiW := chimiddleware.NewWrapResponseWriter(tc.below, tc.proto)
			got := withFlushError(chiW)
			if want, have := optionalInterfaces(chiW), optionalInterfaces(got); want != have {
				t.Fatalf("chi writer [flush hijack readfrom push] = %v, wrapped = %v", want, have)
			}
			if _, fl := chiW.(http.Flusher); fl {
				if _, ok := got.(interface{ FlushError() error }); !ok {
					t.Fatal("a flushing chi writer was not given FlushError")
				}
			}
		})
	}

	// chi's compressor, captured from inside its middleware.
	t.Run("compressor", func(t *testing.T) {
		var compW, got http.ResponseWriter
		h := chimiddleware.Compress(5)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			compW = w
			got = withFlushError(w)
		}))
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(shapeFlushHijackReaderFrom{shapeFlushHijack{shapeFlush{rec()}}}, r)
		if want, have := optionalInterfaces(compW), optionalInterfaces(got); want != have {
			t.Fatalf("compressor [flush hijack readfrom push] = %v, wrapped = %v", want, have)
		}
		if _, ok := got.(interface{ FlushError() error }); !ok {
			t.Fatal("the compressor's writer was not given FlushError")
		}
	})
}
