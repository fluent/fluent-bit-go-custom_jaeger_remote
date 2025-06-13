package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/calyptia/plugin"

	api_v2 "github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

func Init_Server(ctx context.Context, fbit *plugin.Fluentbit, plug *jeagerRemotePlugin) error {
	cfg, err := loadServerConfig(fbit)
	if err != nil {
		plug.log.Error("configuration error: %v", err)
		return err
	}
	plug.config = cfg

	sampler, err := newRemoteSampler(ctx, cfg)
	if err != nil {
		plug.log.Error("could not create remote sampler: %v", err)
		return err
	}
	plug.sampler = sampler

	plug.cache = &samplingStrategyCache{
		strategies: make(map[string]*api_v2.SamplingStrategyResponse),
	}

	go plug.pollStrategiesWithRetry(ctx)

	httpServer := plug.startHttpServer()
	grpcServer, err := plug.startGrpcServer()
	if err != nil {
		plug.log.Error("could not start gRPC server: %v", err)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
		return err
	}

	go func() {
		<-ctx.Done()
		plug.log.Info("context cancelled, shutting down all services...")

		grpcServer.GracefulStop()
		plug.log.Info("gRPC server stopped.")

		sampler.conn.Close()
		plug.log.Info("gRPC client connection to Jaeger Collector closed.")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			plug.log.Error("http server shutdown error: %v", err)
		}
		plug.log.Info("HTTP server stopped.")
	}()

	plug.log.Info("plugin initialized. HTTP on %s, gRPC on %s", cfg.HttpListenAddr, cfg.GrpcListenAddr)
	return nil
}

//
// --- Background Services (Polling, HTTP, gRPC) ---
//

func (plug *jeagerRemotePlugin) pollStrategiesWithRetry(ctx context.Context) {
	if plug.config.Retry == nil {
		plug.log.Error("retry configuration is missing, poller will not start")
		return
	}

	initialInterval := plug.config.Retry.InitialInterval
	maxInterval := plug.config.Retry.MaxInterval
	multiplier := plug.config.Retry.Multiplier
	currentInterval := initialInterval

	timer := time.NewTimer(currentInterval)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			if err := plug.updateCache(ctx); err != nil {
				plug.log.Error("failed to update cache, scheduling retry: %v", err)
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

func (plug *jeagerRemotePlugin) updateCache(ctx context.Context) error {
	plug.log.Info("updating %d sampling strategies...", len(plug.config.ServiceNames))
	var lastErr error
	for _, svcName := range plug.config.ServiceNames {
		grpcResp, err := plug.sampler.client.GetSamplingStrategy(ctx, &api_v2.SamplingStrategyParameters{ServiceName: svcName})
		if err != nil {
			plug.log.Error("could not get strategy for '%s': %v", svcName, err)
			lastErr = err
			continue
		}
		plug.cache.Lock()
		plug.cache.strategies[svcName] = grpcResp
		plug.cache.Unlock()
	}
	return lastErr
}

func (plug *jeagerRemotePlugin) startHttpServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sampling", plug.handleSampling)
	mux.HandleFunc("/strategies", plug.handleGetStrategies)
	server := &http.Server{Addr: plug.config.HttpListenAddr, Handler: mux}
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			plug.log.Error("HTTP server error: %v", err)
		}
	}()
	return server
}

func (plug *jeagerRemotePlugin) startGrpcServer() (*grpc.Server, error) {
	lis, err := net.Listen("tcp", plug.config.GrpcListenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", plug.config.GrpcListenAddr, err)
	}
	s := grpc.NewServer()
	api_v2.RegisterSamplingManagerServer(s, &grpcApiServer{cache: plug.cache, log: plug.log})
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

func (plug *jeagerRemotePlugin) handleSampling(w http.ResponseWriter, r *http.Request) {
	serviceName := r.URL.Query().Get("service")
	if serviceName == "" {
		http.Error(w, "query parameter 'service' is required", http.StatusBadRequest)
		return
	}
	plug.cache.RLock()
	strategy, exists := plug.cache.strategies[serviceName]
	plug.cache.RUnlock()
	if !exists {
		http.Error(w, fmt.Sprintf(`{"error": "strategy not found for service %s"}`, serviceName), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(strategy)
}

func (plug *jeagerRemotePlugin) handleGetStrategies(w http.ResponseWriter, r *http.Request) {
	plug.cache.RLock()
	defer plug.cache.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(plug.cache.strategies)
}

func loadServerConfig(fbit *plugin.Fluentbit) (*Config, error) {
	log := fbit.Logger
cfg := &Config{
		Endpoint:       fbit.Conf.String("endpoint"),
		HttpListenAddr: fbit.Conf.String("http.listen_addr"),
		GrpcListenAddr: fbit.Conf.String("grpc.listen_addr"),
		Headers:        parseHeaders(fbit.Conf.String("headers")),
	}

	// TLS設定の手動パース
	cfg.TLS = TLSClientSettings{
		ServerName: fbit.Conf.String("server_name_override"),
		CAFile:     fbit.Conf.String("ca_file"),
		CertFile:   fbit.Conf.String("cert_file"),
		KeyFile:    fbit.Conf.String("key_file"),
	}
	if insecureStr := fbit.Conf.String("insecure"); insecureStr != "" {
		val, err := strconv.ParseBool(insecureStr)
		if err != nil {
			log.Warn("could not parse 'Insecure' value, defaulting to false. error: %v", err)
			cfg.TLS.Insecure = false
		} else {
			cfg.TLS.Insecure = val
		}
	}

	// Keepalive設定の手動パース
	if kaTimeStr := fbit.Conf.String("keepalive.time"); kaTimeStr != "" {
		cfg.Keepalive = &KeepaliveConfig{}
		val, err := time.ParseDuration(kaTimeStr)
		if err != nil {
			log.Warn("could not parse 'Keepalive.time', skipping keepalive config. error: %v", err)
			cfg.Keepalive = nil
		} else {
			cfg.Keepalive.Time = val

			// Timeout
			if kaTimeoutStr := fbit.Conf.String("keepalive.timeout"); kaTimeoutStr != "" {
				val, err := time.ParseDuration(kaTimeoutStr)
				if err != nil {
					log.Warn("could not parse 'keepalive.timeout', using default of 20s. error: %v", err)
					cfg.Keepalive.Timeout = 20 * time.Second
				} else {
					cfg.Keepalive.Timeout = val
				}
			} else {
				cfg.Keepalive.Timeout = 20 * time.Second // Default
			}

			// PermitWithoutStream
			if kaPermitStr := fbit.Conf.String("keepalive.permit_without_stream"); kaPermitStr != "" {
				val, err := strconv.ParseBool(kaPermitStr)
				if err != nil {
					log.Warn("could not parse 'kKeepalive.permit_without_stream', using default of true. error: %v", err)
					cfg.Keepalive.PermitWithoutStream = true
				} else {
					cfg.Keepalive.PermitWithoutStream = val
				}
			} else {
				cfg.Keepalive.PermitWithoutStream = true // Default
			}
		}
	}

	// Retry設定の手動パース
	cfg.Retry = &RetryConfig{}
	if valStr := fbit.Conf.String("retry.initialInterval"); valStr != "" {
		val, err := time.ParseDuration(valStr)
		if err != nil {
			log.Warn("could not parse 'retry.initialInterval', using default of 5s. error: %v", err)
			cfg.Retry.InitialInterval = 5 * time.Second
		} else {
			cfg.Retry.InitialInterval = val
		}
	} else {
		cfg.Retry.InitialInterval = 5 * time.Second // Default
	}

	if valStr := fbit.Conf.String("retry.max_interval"); valStr != "" {
		val, err := time.ParseDuration(valStr)
		if err != nil {
			log.Warn("could not parse 'retry.max_interval', using default of 5m. error: %v", err)
			cfg.Retry.MaxInterval = 5 * time.Minute
		} else {
			cfg.Retry.MaxInterval = val
		}
	} else {
		cfg.Retry.MaxInterval = 5 * time.Minute // Default
	}

	if valStr := fbit.Conf.String("retry.multiplier"); valStr != "" {
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			log.Warn("could not parse 'retry.multiplier', using default of 1.5. error: %v", err)
			cfg.Retry.Multiplier = 1.5
		} else {
			cfg.Retry.Multiplier = val
		}
	} else {
		cfg.Retry.Multiplier = 1.5 // Default
	}

	if cfg.Retry.Multiplier <= 1.0 {
		log.Warn("'retry.multiplier' must be > 1.0, using default of 1.5")
		cfg.Retry.Multiplier = 1.5
	}

	if cfg.Endpoint == "" {
		return nil, errors.New("'endpoint' is required")
	}
	if cfg.HttpListenAddr == "" {
		return nil, errors.New("'http_listen_addr' is required")
	}
	if cfg.GrpcListenAddr == "" {
		return nil, errors.New("'grpc_listen_addr' is required")
	}
	serviceNamesStr := fbit.Conf.String("service_names")
	if serviceNamesStr == "" {
		return nil, errors.New("'service_names' (comma-separated) is required")
	}
	for _, s := range strings.Split(serviceNamesStr, ",") {
		cfg.ServiceNames = append(cfg.ServiceNames, strings.TrimSpace(s))
	}
	return cfg, nil
}

func newRemoteSampler(ctx context.Context, cfg *Config) (*remoteSampler, error) {
	tlsConfig, err := loadTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(tlsConfig)
	if cfg.TLS.Insecure {
		creds = insecure.NewCredentials()
	}
	var dialOpts []grpc.DialOption
	dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
	if cfg.Keepalive != nil {
		dialOpts = append(dialOpts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.Keepalive.Time,
			Timeout:             cfg.Keepalive.Timeout,
			PermitWithoutStream: cfg.Keepalive.PermitWithoutStream,
		}))
	}
	if len(cfg.Headers) > 0 {
		dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.NewOutgoingContext(ctx, metadata.New(cfg.Headers)), method, req, reply, cc, opts...)
		}))
	}
	conn, err := grpc.DialContext(ctx, cfg.Endpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial to jaeger collector failed: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("grpc dial to jaeger collector failed: %w", err)
	}
	client := api_v2.NewSamplingManagerClient(conn)
	return &remoteSampler{conn: conn, client: client}, nil
}

func loadTLSConfig(cfg TLSClientSettings) (*tls.Config, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: cfg.Insecure, ServerName: cfg.ServerName}
	if cfg.CAFile != "" {
		caBytes, err := ioutil.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		caPool := x509.NewCertPool()
		caPool.AppendCertsFromPEM(caBytes)
		tlsConfig.RootCAs = caPool
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return tlsConfig, nil
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
