package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Serve runs the relay on addr until ctx ends, then shuts down gracefully:
// open streams are cancelled and in-flight requests get 10 seconds.
func Serve(ctx context.Context, addr string, s *Server) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ServeListener(ctx, ln, s)
}

// ServeListener is Serve on an existing listener.
func ServeListener(ctx context.Context, ln net.Listener, s *Server) error {
	base, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	srv := &http.Server{
		Handler:           withTimeouts(s.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	srv.RegisterOnShutdown(cancelStreams)

	gcCtx, stopGC := context.WithCancel(ctx)
	defer stopGC()
	go s.RunGC(gcCtx)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.opts.Logger.Info("relay listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	return nil
}

// withTimeouts gives ordinary requests a 30-second write deadline. The SSE
// handler clears it for its own stream.
func withTimeouts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		}
		next.ServeHTTP(w, r)
	})
}

// compactJSON writes src without insignificant whitespace.
func compactJSON(dst *strings.Builder, src []byte) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, src); err != nil {
		return err
	}
	dst.Write(buf.Bytes())
	return nil
}
