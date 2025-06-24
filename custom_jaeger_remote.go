// Copyright The Fluent Bit Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/calyptia/plugin"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	api_v2 "github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

func init() {
	plugin.RegisterCustom("jaeger_remote", "Jaeger remote sampling", &jaegerRemotePlugin{})
}

//
// --- Plugin Data Structures ---
//

type jaegerRemotePlugin struct {
	log          plugin.Logger
	config       *Config
	server       *serverComponent // Server-related state
	clientTracer *clientComponent // Client-related state

	// newSamplerFn allows injecting a mock sampler factory for testing.
	newSamplerFn func(context.Context, *Config) (*remoteSampler, error)
	reload       chan struct{}

	// startHttpServerFn allows injecting a mock http server starter for testing.
	startHttpServerFn func(p *jaegerRemotePlugin) *http.Server

	wgClient    *sync.WaitGroup
	wgServer    *sync.WaitGroup
	wgCache     *sync.WaitGroup
	wgLifecycle *sync.WaitGroup
}

type serverComponent struct {
	sampler    *remoteSampler
	cache      *samplingStrategyCache
	httpServer *http.Server
	grpcServer *grpc.Server
}

type clientComponent struct {
	tracerProvider *sdktrace.TracerProvider
}

type CorsSettings struct {
	AllowedOrigins []string
}

type Config struct {
	Mode    string // "client", "server", or "all"
	Headers map[string]string
	// Client-specific settings
	ClientServerURL   string
	ClientSamplingURL string
	ClientRate        time.Duration
	// Server-specific settings
	ServerEndpoint       string
	ServerStrategyFile   string
	ServerHttpListenAddr string
	ServerGrpcListenAddr string
	ServerCors           CorsSettings
	ServerServiceNames   []string
	ServerTLS            TLSSettings
	ServerHeaders        map[string]string
	ServerKeepalive      *KeepaliveConfig
	ServerRetry          *RetryConfig
	ServerReloadInterval time.Duration
}

type KeepaliveConfig struct {
	Time, Timeout       time.Duration
	PermitWithoutStream bool
}
type RetryConfig struct {
	InitialInterval, MaxInterval time.Duration
	MaxRetry                     int64
	Multiplier                   float64
}
type TLSSettings struct {
	Insecure                              bool
	ServerName, CAFile, CertFile, KeyFile string
}
type remoteSampler struct {
	client api_v2.SamplingManagerClient
	conn   *grpc.ClientConn
}
type cacheEntry struct {
	strategy   *api_v2.SamplingStrategyResponse
	expires_at time.Time
}

type samplingStrategyCache struct {
	sync.RWMutex
	strategies map[string]*cacheEntry
}
type grpcApiServer struct {
	api_v2.UnimplementedSamplingManagerServer
	plug *jaegerRemotePlugin
}

//
// --- Main Plugin Initialization ---
//

func (plug *jaegerRemotePlugin) Init(ctx context.Context, fbit *plugin.Fluentbit) error {
	if plug.reload != nil {
		plug.log.Info("Re-initializing plugin, shutting down the previous instance...")
		plug.cleanupOnReload()
	}
	plug.log = fbit.Logger
	cfg, err := loadConfig(fbit)
	if err != nil {
		plug.log.Error("configuration error: %v", err)
		return err
	}
	plug.config = cfg

	if plug.wgClient == nil {
		plug.wgClient = &sync.WaitGroup{}
	}
	if plug.wgServer == nil {
		plug.wgServer = &sync.WaitGroup{}
	}
	if plug.wgCache == nil {
		plug.wgCache = &sync.WaitGroup{}
	}
	if plug.wgLifecycle == nil {
		plug.wgLifecycle = &sync.WaitGroup{}
	}
	plug.reload = make(chan struct{})

	// Default to the real sampler factory if none is injected for tests.
	if plug.newSamplerFn == nil {
		plug.newSamplerFn = newRemoteSampler
	}

	if plug.startHttpServerFn == nil {
		// Use the real HTTP server starter by default.
		plug.startHttpServerFn = (*jaegerRemotePlugin).startHttpServer
	}

	if cfg.Mode == "client" || cfg.Mode == "all" {
		if err := plug.initClient(ctx); err != nil {
			return fmt.Errorf("failed to initialize client mode: %w", err)
		}
	}
	if cfg.Mode == "server" || cfg.Mode == "all" {
		if err := plug.initServer(ctx); err != nil {
			return fmt.Errorf("failed to initialize server mode: %w", err)
		}
	}
	plug.log.Info("plugin initialized successfully in mode: '%s'", cfg.Mode)
	plug.wgLifecycle.Add(1)
	go func() {
		defer plug.wgLifecycle.Done()
		<-ctx.Done()
		plug.log.Debug("Context cancelled, shutting down plugin instance...")
	}()

	return nil
}

func (plug *jaegerRemotePlugin) cleanupOnReload() {
	// Signal all background goroutines to stop.
	// Use a non-blocking close to prevent panic if called multiple times.
	select {
	case <-plug.reload:
		// already closed
	default:
		close(plug.reload)
	}

	// --- Shutdown Server Components ---
	if plug.server != nil {
		if plug.server.grpcServer != nil {
			plug.server.grpcServer.GracefulStop()
			plug.log.Info("gRPC server stopped.")
		}
		if plug.server.httpServer != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := plug.server.httpServer.Shutdown(shutdownCtx); err != nil {
				plug.log.Error("http server shutdown error: %v", err)
			}
			plug.log.Info("HTTP server stopped.")
		}
	}

	if plug.clientTracer != nil {
		plug.wgClient.Wait()
	}
	plug.wgServer.Wait()

	// --- Shutdown Client Components ---
	if plug.clientTracer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plug.clientTracer.tracerProvider.Shutdown(shutdownCtx); err != nil {
			plug.log.Error("failed to shutdown tracer provider: %v", err)
		}
		plug.log.Info("Client tracer provider shut down.")
	}

	if plug.server != nil {
		plug.wgCache.Wait()
	}

	// --- Shutdown Server Components ---
	if plug.server != nil {
		if plug.server.sampler != nil && plug.server.sampler.conn != nil {
			_ = plug.server.sampler.conn.Close()
			plug.log.Info("gRPC client connection to Jaeger Collector closed.")
		}
	}

	// Reset state
	plug.server = nil
	plug.clientTracer = nil
	plug.reload = nil // Mark as reloading

	plug.log.Info("Previous plugin instance cleaned up successfully.")
}

//
// --- Helper Functions ---
//

func newRemoteSampler(ctx context.Context, cfg *Config) (*remoteSampler, error) {
	tlsConfig, err := loadTLSConfig(cfg.ServerTLS)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(tlsConfig)
	if cfg.ServerTLS.Insecure {
		creds = insecure.NewCredentials()
	}

	var dialOpts []grpc.DialOption
	dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))

	if cfg.ServerKeepalive != nil {
		dialOpts = append(dialOpts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: cfg.ServerKeepalive.Time, Timeout: cfg.ServerKeepalive.Timeout, PermitWithoutStream: cfg.ServerKeepalive.PermitWithoutStream,
		}))
	}
	if len(cfg.ServerHeaders) > 0 {
		dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(metadata.NewOutgoingContext(ctx, metadata.New(cfg.ServerHeaders)), method, req, reply, cc, opts...)
		}))
	}

	conn, err := grpc.NewClient(cfg.ServerEndpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc newclient to jaeger collector failed: %w", err)
	}

	dialCtx, connectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer connectCancel()

	conn.Connect()

	if err != nil {
		return nil, fmt.Errorf("grpc newclient config failed: %w", err)
	}

	client := api_v2.NewSamplingManagerClient(conn)

	s := conn.GetState()
	if s != connectivity.Ready {
		if !conn.WaitForStateChange(dialCtx, s) {
			return nil, fmt.Errorf("gRPC connection did not become ready within timeout. Last state: %s", conn.GetState())
		}
	}

	return &remoteSampler{conn: conn, client: client}, nil
}

func main() {
}
