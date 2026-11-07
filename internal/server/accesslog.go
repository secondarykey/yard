package server

import (
	"bufio"
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"yard/internal/proc"
)

// accessLog writes Combined Log Format followed by the processing time (ms) and the target server name.
//
//	127.0.0.1 - - [07/Nov/2026:17:00:00 +0900] "GET /memo/ HTTP/1.1" 200 1234 "-" "curl/8" 12 memo
type accessLog struct {
	f *os.File
	l *log.Logger
}

func openAccessLog(name string) (*accessLog, error) {
	if name == "" {
		return nil, nil
	}
	f, err := proc.OpenLog(name)
	if err != nil {
		return nil, err
	}
	return &accessLog{f: f, l: log.New(f, "", 0)}, nil
}

func (a *accessLog) Close() {
	if a != nil {
		a.f.Close()
	}
}

type targetKey struct{}

// setTarget records the target server name for the access log.
func setTarget(r *http.Request, name string) {
	if p, ok := r.Context().Value(targetKey{}).(*string); ok {
		*p = name
	}
}

func (s *Server) logAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		a := s.access
		s.mu.RUnlock()
		if a == nil {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		target := new(string)
		r = r.WithContext(context.WithValue(r.Context(), targetKey{}, target))
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if rec.hijacked && status == 0 {
			status = http.StatusSwitchingProtocols
		} else if status == 0 {
			status = http.StatusOK
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		a.l.Printf("%s - - [%s] %q %d %d %q %q %d %s",
			host,
			start.Format("02/Jan/2006:15:04:05 -0700"),
			r.Method+" "+r.RequestURI+" "+r.Proto,
			status,
			rec.bytes,
			orDash(r.Referer()),
			orDash(r.UserAgent()),
			time.Since(start).Milliseconds(),
			orDash(*target),
		)
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// recorder records the status and the number of bytes sent.
// Flush reaches the underlying ResponseWriter via Unwrap and http.ResponseController.
// Only Hijack is handled here, to record it (so that WebSocket 101s are logged).
type recorder struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 && code >= 200 || code == http.StatusSwitchingProtocols {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Flush() {
	http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.hijacked = true
	}
	return c, rw, err
}
