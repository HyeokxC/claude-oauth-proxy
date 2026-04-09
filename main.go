package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	listenAddr := flag.String("listen", DefaultListenAddr, "address to listen on")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("[main] cc-proxy starting on %s", *listenAddr)
	log.Printf("[main] upstream: %s", UpstreamBase)

	tokenMgr := NewTokenManager()

	// Pre-load token to fail fast
	if _, err := tokenMgr.GetValidToken(); err != nil {
		log.Printf("[main] WARNING: could not load initial token: %v", err)
		log.Printf("[main] will retry on first request")
	} else {
		log.Printf("[main] token loaded successfully")
	}

	handler := NewProxyHandler(tokenMgr)
	server := &http.Server{
		Addr:    *listenAddr,
		Handler: handler,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Printf("[main] shutting down...")
		server.Close()
	}()

	log.Printf("[main] ready. Configure OpenCode with:")
	log.Printf("[main]   ANTHROPIC_BASE_URL=http://%s/v1", *listenAddr)
	log.Printf("[main]   ANTHROPIC_API_KEY=proxy-placeholder")

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("[main] server error: %v", err)
	}
	log.Printf("[main] stopped")
}
