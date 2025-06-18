package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	api_v2 "github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"go.opentelemetry.io/contrib/samplers/jaegerremote"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

//
// --- Client Mode Initialization ---
//

func (plug *jaegerRemotePlugin) initClient(ctx context.Context) error {
	plug.log.Info("initializing client mode...")

	jaegerRemoteSampler := jaegerremote.New(
		"fluent-bit-go",
		jaegerremote.WithSamplingServerURL(plug.config.ClientSamplingURL),
		jaegerremote.WithSamplingRefreshInterval(plug.config.ClientRate),
		jaegerremote.WithInitialSampler(sdktrace.TraceIDRatioBased(0.5)),
	)

	httpClient := otlptracehttp.NewClient(
		otlptracehttp.WithEndpoint(plug.config.ClientServerURL),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
	)
	exporter, err := otlptrace.New(ctx, httpClient)
	if err != nil {
		return err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(jaegerRemoteSampler),
		sdktrace.WithSyncer(exporter),
	)
	otel.SetTracerProvider(tp)

	plug.clientTracer = &clientComponent{tracerProvider: tp}

	go func() {
		ticker := time.Tick(plug.config.ClientRate)
		for {
			<-ticker
			plug.log.Debug("[jaeger_remote] jeager sampling is alive %v", time.Now())
		}
	}()

	go func() {
		<-ctx.Done()
		plug.log.Info("shutting down client tracer provider...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := tp.Shutdown(shutdownCtx); err != nil {
			plug.log.Error("failed to shutdown tracer provider: %v", err)
		}
	}()

	plug.log.Info("client mode initialized, sampling from '%s'", plug.config.ClientSamplingURL)
	return nil
}

// --- Server Mode Initialization ---
func (plug *jaegerRemotePlugin) initServer(ctx context.Context) error {
	plug.log.Info("initializing server mode...")
	plug.server = &serverComponent{}
	plug.server.cache = &samplingStrategyCache{
		strategies: make(map[string]*cacheEntry),
	}

	// Determine strategy source: remote or file
	if plug.config.ServerStrategyFile != "" {
		if err := plug.loadStrategiesFromFile(); err != nil {
			return fmt.Errorf("could not load strategies from file: %w", err)
		}
	} else if plug.config.ServerEndpoint != "" {
		sampler, err := plug.newSamplerFn(ctx, plug.config)
		if err != nil {
			return fmt.Errorf("could not create remote sampler for server: %w", err)
		}
		plug.server.sampler = sampler
	}

	// Start servers only if their listen addresses are configured.
	var err error
	if plug.config.ServerHttpListenAddr != "" {
		plug.server.httpServer = plug.startHttpServer()
	}
	if plug.config.ServerGrpcListenAddr != "" {
		plug.server.grpcServer, err = plug.startGrpcServer()
		if err != nil {
			plug.log.Error("could not start gRPC server: %v", err)
			if plug.server.httpServer != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				_ = plug.server.httpServer.Shutdown(shutdownCtx)
			}
			return err
		}
	}

	if plug.server.httpServer == nil && plug.server.grpcServer == nil {
		return errors.New("server mode is enabled, but neither 'server.http.listen_addr' nor 'server.grpc.listen_addr' are configured")
	}

	go func() {
		<-ctx.Done()
		plug.log.Info("shutting down server components...")
		if plug.server.grpcServer != nil {
			plug.server.grpcServer.GracefulStop()
			plug.log.Info("gRPC server stopped.")
		}
		if plug.server.sampler != nil && plug.server.sampler.conn != nil {
			_ = plug.server.sampler.conn.Close()
			plug.log.Info("gRPC client connection to Jaeger Collector closed.")
		}
		if plug.server.httpServer != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := plug.server.httpServer.Shutdown(shutdownCtx); err != nil {
				plug.log.Error("http server shutdown error: %v", err)
			}
			plug.log.Info("HTTP server stopped.")
		}
	}()

	logMsg := "server mode initialized."
	if plug.server.httpServer != nil {
		logMsg += fmt.Sprintf(" HTTP on %s.", plug.config.ServerHttpListenAddr)
	}
	if plug.server.grpcServer != nil {
		logMsg += fmt.Sprintf(" gRPC on %s.", plug.config.ServerGrpcListenAddr)
	}
	plug.log.Info(logMsg)
	return nil
}

//
// --- Background Services (Polling, HTTP, gRPC) ---
//

func (plug *jaegerRemotePlugin) getAndCacheStrategy(ctx context.Context, serviceName string) (*api_v2.SamplingStrategyResponse, error) {
	plug.server.cache.RLock()
	entry, exists := plug.server.cache.strategies[serviceName]
	if exists && time.Now().Before(entry.expires_at) {
		plug.server.cache.RUnlock()
		plug.log.Debug("cache hit for service: %s", serviceName)
		return entry.strategy, nil
	}
	plug.server.cache.RUnlock()

	plug.server.cache.Lock()
	defer plug.server.cache.Unlock()

	entry, exists = plug.server.cache.strategies[serviceName]
	if exists && time.Now().Before(entry.expires_at) {
		return entry.strategy, nil
	}

	plug.log.Info("cache miss or expired for service '%s', fetching from remote...", serviceName)
	var grpcResp *api_v2.SamplingStrategyResponse
	var err error

	if plug.config.ServerRetry != nil {
		cfg := plug.config.ServerRetry
		currentInterval := cfg.InitialInterval

		for i := 0; i < int(cfg.MaxRetry); i++ {
			grpcResp, err = plug.server.sampler.client.GetSamplingStrategy(ctx, &api_v2.SamplingStrategyParameters{ServiceName: serviceName})
			if err == nil {
				break
			}

			if i == int(cfg.MaxRetry)-1 {
				break
			}

			plug.log.Warn("fetch attempt %d failed for '%s', retrying in %v. error: %v", i+1, serviceName, currentInterval, err)

			select {
			case <-time.After(currentInterval):
			case <-ctx.Done():
				plug.log.Warn("retry cancelled for service '%s' because context was done.", serviceName)
				return nil, ctx.Err()
			}

			nextInterval := time.Duration(float64(currentInterval) * cfg.Multiplier)

			if nextInterval > cfg.MaxInterval {
				plug.log.Debug("backoff interval capped by max_interval. using %v instead of %v", cfg.MaxInterval, nextInterval)
				currentInterval = cfg.MaxInterval
			} else {
				currentInterval = nextInterval
			}
		}
	} else {
		grpcResp, err = plug.server.sampler.client.GetSamplingStrategy(ctx, &api_v2.SamplingStrategyParameters{ServiceName: serviceName})
	}

	if err != nil {
		if exists {
			plug.log.Warn("failed to fetch new strategy for service '%s', returning stale data. error: %v", serviceName, err)
			return entry.strategy, nil
		}
		return nil, fmt.Errorf("failed to fetch strategy for service %s: %w", serviceName, err)
	}

	newEntry := &cacheEntry{
		strategy:   grpcResp,
		expires_at: time.Now().Add(plug.config.ServerReloadInterval),
	}
	plug.server.cache.strategies[serviceName] = newEntry
	plug.log.Info("cache updated for service '%s' with TTL %v", serviceName, plug.config.ServerReloadInterval)

	return newEntry.strategy, nil
}

func (plug *jaegerRemotePlugin) loadStrategiesFromFile() error {
	plug.log.Info("loading sampling strategies from file: %s", plug.config.ServerStrategyFile)
	data, err := ioutil.ReadFile(plug.config.ServerStrategyFile)
	if err != nil {
		return fmt.Errorf("could not read strategy file: %w", err)
	}

	var strategiesFromFile map[string]*api_v2.SamplingStrategyResponse
	if err := json.Unmarshal(data, &strategiesFromFile); err != nil {
		return fmt.Errorf("could not unmarshal strategy file: %w", err)
	}

	plug.server.cache.Lock()
	defer plug.server.cache.Unlock()

	longTTL := 24 * 365 * 10 * time.Hour // 10 years
	for serviceName, strategy := range strategiesFromFile {
		plug.server.cache.strategies[serviceName] = &cacheEntry{
			strategy:   strategy,
			expires_at: time.Now().Add(longTTL),
		}
	}

	plug.log.Info("successfully loaded %d strategies from file", len(strategiesFromFile))
	return nil
}

func (plug *jaegerRemotePlugin) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(plug.config.ServerCors.AllowedOrigins) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		isAllowed := false
		for _, allowed := range plug.config.ServerCors.AllowedOrigins {
			if allowed == "*" || allowed == origin {
				isAllowed = true
				break
			}
		}

		if isAllowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}

		// Handle pre-flight OPTIONS request
		if r.Method == "OPTIONS" {
			if isAllowed {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// If not an allowed origin, forbid the request.
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// Serve the actual request for GET, etc.
		next.ServeHTTP(w, r)
	})
}

func (plug *jaegerRemotePlugin) startHttpServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sampling", plug.handleSampling)
	mux.HandleFunc("/strategies", plug.handleGetStrategies)

	server := &http.Server{Addr: plug.config.ServerHttpListenAddr, Handler: plug.corsMiddleware(mux)} //
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			plug.log.Error("HTTP server error: %v", err)
		}
	}()
	return server
}

func (plug *jaegerRemotePlugin) startGrpcServer() (*grpc.Server, error) {
	lis, err := net.Listen("tcp", plug.config.ServerGrpcListenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", plug.config.ServerGrpcListenAddr, err)
	}
	s := grpc.NewServer()
	// Pass the entire plugin to the grpcApiServer
	api_v2.RegisterSamplingManagerServer(s, &grpcApiServer{plug: plug})
	reflection.Register(s)
	go func() {
		if err := s.Serve(lis); err != nil {
			plug.log.Error("gRPC server failed to serve: %v", err)
		}
	}()
	return s, nil
}
func (s *grpcApiServer) GetSamplingStrategy(ctx context.Context, params *api_v2.SamplingStrategyParameters) (*api_v2.SamplingStrategyResponse, error) {
	serviceName := params.GetServiceName()
	if serviceName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "service_name is required")
	}

	s.plug.log.Debug("gRPC request for sampling strategy received for service: %s", serviceName)

	strategy, err := s.plug.getAndCacheStrategy(ctx, serviceName)
	if err != nil {
		// Return an appropriate gRPC error
		return nil, status.Errorf(codes.NotFound, "strategy not found for service %s: %v", serviceName, err)
	}

	return strategy, nil
}

func (plug *jaegerRemotePlugin) handleSampling(w http.ResponseWriter, r *http.Request) {
	serviceName := r.URL.Query().Get("service")
	if serviceName == "" {
		http.Error(w, "query parameter 'service' is required", http.StatusBadRequest)
		return
	}

	strategy, err := plug.getAndCacheStrategy(r.Context(), serviceName)
	if err != nil {
		// This uses the status codes from the grpc-go library to map gRPC errors to HTTP status codes.
		http.Error(w, fmt.Sprintf(`{"error": "strategy not found for service %s: %v"}`, serviceName, err), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(strategy)
}

func (plug *jaegerRemotePlugin) handleGetStrategies(w http.ResponseWriter, r *http.Request) {
	plug.server.cache.RLock()
	defer plug.server.cache.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(plug.server.cache.strategies)
}

func parseHeaders(h string) map[string]string {
	if h == "" {
		return nil
	}
	m := make(map[string]string)
	for _, p := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	return m
}
