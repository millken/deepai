package gateway

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"sync/atomic"
	"time"
)

type contextKey string

const requestIDContextKey contextKey = "request_id"

var requestSeq uint64

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.withRecover(s.withLogging(s.withInFlight(s.withRequestID(next))))
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("X-Request-Id", requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.logger.Printf("%s %s status=%d duration=%s request_id=%s", r.Method, r.URL.Path, rw.status, time.Since(started).Round(time.Millisecond), requestIDFromContext(r.Context()))
	})
}

func (s *Server) withInFlight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("server is shutting down"))
			return
		}

		s.inFlight.Add(1)
		atomic.AddInt64(&s.inFlightCount, 1)
		defer func() {
			atomic.AddInt64(&s.inFlightCount, -1)
			s.inFlight.Done()
		}()

		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Printf("panic request_id=%s err=%v", requestIDFromContext(r.Context()), recovered)
				writeError(w, http.StatusInternalServerError, fmt.Errorf("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey).(string)
	return requestID
}

func newRequestID() string {
	var random [6]byte
	_, _ = rand.Read(random[:])
	return fmt.Sprintf("%d-%s", atomic.AddUint64(&requestSeq, 1), hex.EncodeToString(random[:]))
}
