package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

const (
	proxyHost      = "proxy.wonderproxy.com"
	proxyPort      = 443
	listenAddress  = ":8443"
	bettingAPIBase = "https://api.bettingprovider.com"
	maxConnections = 10000
	requestTimeout = 30 * time.Second
)

type ProxyServer struct {
	client      *http.Client
	activeConns int
	connLock    sync.Mutex
	wg          sync.WaitGroup
	limiter     *rate.Limiter
}

func NewProxyServer() *ProxyServer {
	proxyURL, err := url.Parse(fmt.Sprintf("https://%s:%d", proxyHost, proxyPort))
	if err != nil {
		log.Fatalf("Failed to parse proxy URL: %v", err)
	}

	return &ProxyServer{
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
			Timeout: requestTimeout,
		},
		activeConns: 0,
		limiter:     rate.NewLimiter(rate.Limit(100), 200), // 100 requests per second, burst of 200
	}
}

func (ps *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !ps.limiter.Allow() {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	ps.connLock.Lock()
	if ps.activeConns >= maxConnections {
		ps.connLock.Unlock()
		http.Error(w, "Too many connections, please try again later.", http.StatusServiceUnavailable)
		return
	}
	ps.activeConns++
	ps.connLock.Unlock()

	ps.wg.Add(1)
	defer func() {
		ps.connLock.Lock()
		ps.activeConns--
		ps.connLock.Unlock()
		ps.wg.Done()
	}()

	if err := ps.routeRequest(w, r); err != nil {
		log.Printf("Error processing request: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func (ps *ProxyServer) routeRequest(w http.ResponseWriter, r *http.Request) error {
	apiEndpoint := map[string]string{
		"/odds":    "/odds",
		"/bet":     "/placebet",
		"/results": "/results",
	}

	path := r.URL.Path
	bettingAPIEndpoint, ok := apiEndpoint[path]
	if !ok {
		return fmt.Errorf("unsupported endpoint: %s", path)
	}

	targetURL := fmt.Sprintf("%s%s", bettingAPIBase, bettingAPIEndpoint)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Copy headers, but filter out sensitive ones
	for k, v := range r.Header {
		if !strings.EqualFold(k, "Authorization") && !strings.EqualFold(k, "Cookie") {
			req.Header[k] = v
		}
	}

	resp, err := ps.client.Do(req)
	if err != nil {
		return fmt.Errorf("error contacting betting API: %v", err)
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to send response: %v", err)
	}

	return nil
}

func (ps *ProxyServer) Start() {
	server := &http.Server{
		Addr:    listenAddress,
		Handler: ps,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	go func() {
		log.Printf("Starting sports betting proxy server on %s with max %d concurrent connections", listenAddress, maxConnections)
		if err := server.ListenAndServeTLS("cert.pem", "key.pem"); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	ps.wg.Wait()
	log.Println("Server stopped")
}

func main() {
	proxyServer := NewProxyServer()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			proxyServer.connLock.Lock()
			log.Printf("Active connections: %d\n", proxyServer.activeConns)
			proxyServer.connLock.Unlock()
		}
	}()

	proxyServer.Start()
}

