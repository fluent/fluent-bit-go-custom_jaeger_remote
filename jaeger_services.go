package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		jaegerremote.WithSamplingRefreshInterval(10*time.Second),
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
		<-ctx.Done()
		plug.log.Info("shutting down client tracer provider...")
		if err := tp.Shutdown(ctx); err != nil {
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
	sampler, err := plug.newSamplerFn(ctx, plug.config)
	if err != nil {
		return fmt.Errorf("could not create remote sampler for server: %w", err)
	}
	plug.server.sampler = sampler
	plug.server.cache = &samplingStrategyCache{strategies: make(map[string]*api_v2.SamplingStrategyResponse)}

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

	// All checks passed, now start background goroutines.
	go plug.pollStrategiesWithRetry(ctx)
	go func() {
		<-ctx.Done()
		plug.log.Info("shutting down server components...")
		if plug.server.grpcServer != nil {
			plug.server.grpcServer.GracefulStop()
			plug.log.Info("gRPC server stopped.")
		}
		if sampler.conn != nil {
			_ = sampler.conn.Close()
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

func (plug *jaegerRemotePlugin) pollStrategiesWithRetry(ctx context.Context) {
	cfg := plug.config
	if cfg.ServerRetry == nil {
		plug.log.Error("retry configuration is missing, poller will not start")
		return
	}
	initialInterval, maxInterval, multiplier := cfg.ServerRetry.InitialInterval, cfg.ServerRetry.MaxInterval, cfg.ServerRetry.Multiplier
	currentInterval := initialInterval
	timer := time.NewTimer(currentInterval)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			if err := plug.updateCache(ctx); err != nil {
				plug.log.Error("failed to update cache, scheduling retry in %v: %v", currentInterval, err)
				currentInterval = time.Duration(float64(currentInterval) * multiplier)
				if currentInterval > maxInterval {
					currentInterval = maxInterval
				}
			} else {
				currentInterval = initialInterval
			}
			timer.Reset(currentInterval)
		case <-ctx.Done():
			plug.log.Info("poller stopped.")
			return
		}
	}
}

func (plug *jaegerRemotePlugin) updateCache(ctx context.Context) error {
	plug.log.Info("server updating %d sampling strategies...", len(plug.config.ServerServiceNames))
	var lastErr error
	for _, svcName := range plug.config.ServerServiceNames {
		grpcResp, err := plug.server.sampler.client.GetSamplingStrategy(ctx, &api_v2.SamplingStrategyParameters{ServiceName: svcName})
		if err != nil {
			lastErr = err
			continue
		}
		plug.server.cache.Lock()
		plug.server.cache.strategies[svcName] = grpcResp
		plug.server.cache.Unlock()
	}
	return lastErr
}

func (plug *jaegerRemotePlugin) startHttpServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sampling", plug.handleSampling)
	mux.HandleFunc("/strategies", plug.handleGetStrategies)
	server := &http.Server{Addr: plug.config.ServerHttpListenAddr, Handler: mux}
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
	api_v2.RegisterSamplingManagerServer(s, &grpcApiServer{cache: plug.server.cache, log: plug.log})
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
	s.log.Debug("gRPC request for sampling strategy received for service: %s", serviceName)
	s.cache.RLock()
	strategy, exists := s.cache.strategies[serviceName]
	s.cache.RUnlock()
	if !exists {
		return nil, status.Errorf(codes.NotFound, "no strategy found for service: %s", serviceName)
	}
	return strategy, nil
}

func (plug *jaegerRemotePlugin) handleSampling(w http.ResponseWriter, r *http.Request) {
	serviceName := r.URL.Query().Get("service")
	if serviceName == "" {
		http.Error(w, "query parameter 'service' is required", http.StatusBadRequest)
		return
	}
	plug.server.cache.RLock()
	strategy, exists := plug.server.cache.strategies[serviceName]
	plug.server.cache.RUnlock()
	if !exists {
		http.Error(w, fmt.Sprintf(`{"error": "strategy not found for service %s"}`, serviceName), http.StatusNotFound)
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
