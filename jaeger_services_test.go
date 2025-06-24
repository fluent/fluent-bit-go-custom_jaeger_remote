// Copyright The Fluent Bit Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alecthomas/assert/v2"
	"github.com/calyptia/plugin"
	api_v2 "github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type observedCall struct {
	Metadata metadata.MD
	Params   *api_v2.SamplingStrategyParameters
}

type samplingServer struct {
	api_v2.UnimplementedSamplingManagerServer
	mu              sync.Mutex
	observedCalls   []observedCall
	strategy        *api_v2.SamplingStrategyResponse
	err             error
	actualCallCount int32
}

func (s *samplingServer) GetSamplingStrategy(ctx context.Context, params *api_v2.SamplingStrategyParameters) (*api_v2.SamplingStrategyResponse, error) {
	atomic.AddInt32(&s.actualCallCount, 1) // Use atomic for safe concurrent increments

	md, _ := metadata.FromIncomingContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.observedCalls = append(s.observedCalls, observedCall{Metadata: md.Copy(), Params: params})

	if s.err != nil {
		return nil, s.err
	}
	return s.strategy, nil
}

func (s *samplingServer) getCallCount() int {
	return int(atomic.LoadInt32(&s.actualCallCount))
}

func (s *samplingServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.observedCalls)
}

// lastCall returns a *copy* of the most recently observed call to prevent data races.
func (s *samplingServer) lastCall() (observedCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.observedCalls) == 0 {
		return observedCall{}, false
	}
	return s.observedCalls[len(s.observedCalls)-1], true
}

func poll(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", d, msg)
}

func startMockGrpcServer(t *testing.T, mock *samplingServer) (*grpc.Server, *bufconn.Listener) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	api_v2.RegisterSamplingManagerServer(s, mock)
	go func() {
		if err := s.Serve(lis); err != nil {
			t.Logf("Mock gRPC server stopped: %v", err)
		}
	}()
	return s, lis
}

func startMockHTTPSamplingServer(t *testing.T, strategy *api_v2.SamplingStrategyResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(strategy)
		assert.NoError(t, err)
	}))
}

func Test_InitServer_FileStrategy(t *testing.T) {
	t.Run("successfully initializes server using a strategy file", func(t *testing.T) {
		var wgServer, wgLifecycle sync.WaitGroup
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		strategyJSON := `{
			"service-a": {
				"strategyType": 0,
				"probabilisticSampling": { "samplingRate": 0.5 }
			},
			"service-b": {
				"strategyType": 1,
				"rateLimitingSampling": { "maxTracesPerSecond": 10 }
			}
		}`
		tmpFile, err := os.CreateTemp("", "strategy-*.json")
		assert.NoError(t, err)
		defer os.Remove(tmpFile.Name())
		_, err = tmpFile.Write([]byte(strategyJSON))
		assert.NoError(t, err)
		err = tmpFile.Close()
		assert.NoError(t, err)

		httpListenAddr := getFreePort(t)

		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.strategy_file":    tmpFile.Name(),
				"server.http.listen_addr": httpListenAddr,
			},
		}
		plug := &jaegerRemotePlugin{wgServer: &wgServer, wgLifecycle: &wgLifecycle}
		err = plug.Init(ctx, fbit)
		assert.NoError(t, err)

		assert.NotZero(t, plug.server.cache)
		assert.Equal(t, 2, len(plug.server.cache.strategies))

		req := httptest.NewRequest(http.MethodGet, "/sampling?service=service-a", nil)
		rr := httptest.NewRecorder()
		plug.handleSampling(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp api_v2.SamplingStrategyResponse
		err = json.NewDecoder(rr.Body).Decode(&resp)
		assert.NoError(t, err)
		assert.Equal(t, 0.5, resp.ProbabilisticSampling.GetSamplingRate())

		cancel()
		wgServer.Wait()
		wgLifecycle.Wait()
	})
}

func Test_InitServer_FileStrategyErrors(t *testing.T) {
	t.Run("fails when strategy file does not exist", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf:   mapConfigLoader{"mode": "server", "server.strategy_file": "/tmp/non-existent-file.json"},
		}
		plug := &jaegerRemotePlugin{}
		err := plug.Init(context.Background(), fbit)
		assert.Error(t, err)
	})

	t.Run("fails when strategy file contains invalid json", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "strategy-*.json")
		assert.NoError(t, err)
		defer os.Remove(tmpFile.Name())
		_, err = tmpFile.Write([]byte(`{ "invalid-json`))
		assert.NoError(t, err)
		tmpFile.Close()

		httpListenAddr := getFreePort(t)

		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                 "server",
				"server.strategy_file": tmpFile.Name(),
				"server.http.listen_addr": httpListenAddr,
			},
		}
		plug := &jaegerRemotePlugin{}
		err = plug.Init(context.Background(), fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "could not unmarshal")
	})
}

func Test_InitClient(t *testing.T) {
	t.Run("successfully initializes in client mode", func(t *testing.T) {
		var wgLifecycle sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		mockStrategy := &api_v2.SamplingStrategyResponse{
			StrategyType:          api_v2.SamplingStrategyType_PROBABILISTIC,
			ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{SamplingRate: 0.1},
		}
		mockSamplingSrv := startMockHTTPSamplingServer(t, mockStrategy)
		defer mockSamplingSrv.Close()

		mockOtlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer mockOtlpSrv.Close()

		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                "client",
				"client.server_url":   mockOtlpSrv.URL,
				"client.sampling_url": mockSamplingSrv.URL,
			},
		}
		plug := &jaegerRemotePlugin{wgLifecycle: &wgLifecycle}

		err := plug.Init(ctx, fbit)
		assert.NoError(t, err)

		if plug.clientTracer == nil {
			t.Fatal("clientTracer should be initialized but is nil")
		}
		if plug.clientTracer.tracerProvider == nil {
			t.Fatal("tracerProvider should be initialized but is nil")
		}

		// Explicitly cancel and wait to ensure goroutines are cleaned up before test exits.
		cancel()
		wgLifecycle.Wait()
	})
}

func Test_startProactiveCacheWarmer(t *testing.T) {
	t.Run("proactive warmer fetches strategies on startup and periodically", func(t *testing.T) {
		var wgServer, wgCache, wgLifecycle sync.WaitGroup
		ctx, cancel := context.WithCancel(context.Background())
		// No defer, we cancel manually to control shutdown sequence

		mockStrategy := &api_v2.SamplingStrategyResponse{StrategyType: api_v2.SamplingStrategyType_PROBABILISTIC, ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{SamplingRate: 0.1}}
		mockJaeger := &samplingServer{strategy: mockStrategy}
		upstreamJaegerServer, lis := startMockGrpcServer(t, mockJaeger)
		defer upstreamJaegerServer.Stop()

		httpListenAddr := getFreePort(t)

		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                          "server",
				"server.endpoint":               "passthrough:///bufnet",
				"server.http.listen_addr":       httpListenAddr,
				"server.service_names":          "service-a, service-b", // Two services to warm up
				"server.reload_interval":        "50ms",                 // Very short interval for testing
				"server.retry.initial_interval": "10ms",
			},
		}
		plug := &jaegerRemotePlugin{wgServer: &wgServer, wgCache: &wgCache, wgLifecycle: &wgLifecycle}

		plug.newSamplerFn = func(ctx context.Context, cfg *Config) (*remoteSampler, error) {
			dialOpts := []grpc.DialOption{
				grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			}
			conn, err := grpc.NewClient(cfg.ServerEndpoint, dialOpts...)
			assert.NoError(t, err)

			client := api_v2.NewSamplingManagerClient(conn)
			return &remoteSampler{conn: conn, client: client}, nil
		}

		err := plug.Init(ctx, fbit)
		assert.NoError(t, err)

		// Check initial warm-up
		poll(t, 200*time.Millisecond, func() bool {
			return mockJaeger.getCallCount() >= 2
		}, "expected at least 2 calls for initial cache warm-up")

		// Check periodic refresh
		poll(t, 200*time.Millisecond, func() bool {
			return mockJaeger.getCallCount() >= 4
		}, "expected at least 4 calls after one refresh")

		// Explicitly cancel and wait to ensure goroutines are cleaned up.
		cancel()
		wgServer.Wait()
		wgCache.Wait()
		wgLifecycle.Wait()
	})
}

func Test_InitServer_EndToEnd(t *testing.T) {
	testCases := []struct {
		name            string
		configHeaders   string
		expectedHeaders map[string]string
	}{
		{"no headers", "", nil},
		{
			"with headers",
			"x-custom-header=fluent-bit,authorization=Bearer 12345",
			map[string]string{"x-custom-header": "fluent-bit", "authorization": "Bearer 12345"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var wgServer, wgCache, wgLifecycle sync.WaitGroup
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			mockStrategy := &api_v2.SamplingStrategyResponse{StrategyType: api_v2.SamplingStrategyType_RATE_LIMITING, RateLimitingSampling: &api_v2.RateLimitingSamplingStrategy{MaxTracesPerSecond: 100}}
			mockJaeger := &samplingServer{strategy: mockStrategy}
			upstreamJaegerServer, lis := startMockGrpcServer(t, mockJaeger)
			defer upstreamJaegerServer.Stop()

			httpListenAddr := getFreePort(t)

			fbit := &plugin.Fluentbit{
				Logger: newTestLogger(t),
				Conf: mapConfigLoader{
					"mode":                          "server",
					"server.endpoint":               "passthrough:///bufnet",
					"server.http.listen_addr":       httpListenAddr,
					"server.service_names":          "test-service",
					"server.retry.initial_interval": "10ms",
					"server.headers":                tc.configHeaders,
				},
			}
			plug := &jaegerRemotePlugin{wgServer: &wgServer, wgCache: &wgCache, wgLifecycle: &wgLifecycle}

			plug.newSamplerFn = func(ctx context.Context, cfg *Config) (*remoteSampler, error) {
				dialOpts := []grpc.DialOption{
					grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				}
				if len(cfg.ServerHeaders) > 0 {
					dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
						return invoker(metadata.NewOutgoingContext(ctx, metadata.New(cfg.ServerHeaders)), method, req, reply, cc, opts...)
					}))
				}
				conn, err := grpc.NewClient(cfg.ServerEndpoint, dialOpts...)
				assert.NoError(t, err)

				client := api_v2.NewSamplingManagerClient(conn)
				return &remoteSampler{conn: conn, client: client}, nil
			}

			// Call Init directly instead of in a racy goroutine.
			err := plug.Init(ctx, fbit)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("plug.Init() failed unexpectedly: %v", err)
			}

			poll(t, 2*time.Second, func() bool { return mockJaeger.getCallCount() >= 1 }, "proactive cache warmer did not run")

			resp, err := http.Get(fmt.Sprintf("http://%s/sampling?service=test-service", httpListenAddr))
			assert.NoError(t, err)

			defer resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)

			assert.NotZero(t, mockJaeger.callCount(), "on-demand fetch should have called GetSamplingStrategy")
			lastCall, ok := mockJaeger.lastCall()
			assert.True(t, ok, "expected at least one call to have been observed")
			assert.Equal(t, "test-service", lastCall.Params.ServiceName)

			md := lastCall.Metadata
			assert.True(t, ok)
			for k, v := range tc.expectedHeaders {
				assert.Equal(t, []string{v}, md.Get(k))
			}
			cancel()
			wgServer.Wait()
			wgCache.Wait()
			wgLifecycle.Wait()
		})
	}
}

/* Helper function to mock sampling manager client */

type mockSamplingClient struct {
	api_v2.SamplingManagerClient
	GetSamplingStrategyFunc func(ctx context.Context, in *api_v2.SamplingStrategyParameters, opts ...grpc.CallOption) (*api_v2.SamplingStrategyResponse, error)
}

func (m *mockSamplingClient) GetSamplingStrategy(ctx context.Context, in *api_v2.SamplingStrategyParameters, opts ...grpc.CallOption) (*api_v2.SamplingStrategyResponse, error) {
	if m.GetSamplingStrategyFunc != nil {
		return m.GetSamplingStrategyFunc(ctx, in, opts...)
	}
	return nil, status.Error(codes.Unimplemented, "method GetSamplingStrategy not implemented")
}

func Test_getAndCacheStrategy_Retry(t *testing.T) {
	t.Run("should retry on failure and eventually succeed", func(t *testing.T) {
		var callCount int
		mockSuccessStrategy := &api_v2.SamplingStrategyResponse{StrategyType: api_v2.SamplingStrategyType_PROBABILISTIC}
		mockErr := status.Error(codes.Unavailable, "server not ready")

		mockClient := &mockSamplingClient{
			GetSamplingStrategyFunc: func(ctx context.Context, in *api_v2.SamplingStrategyParameters, opts ...grpc.CallOption) (*api_v2.SamplingStrategyResponse, error) {
				callCount++
				if callCount > 2 {
					return mockSuccessStrategy, nil
				}
				return nil, mockErr
			},
		}

		plug := &jaegerRemotePlugin{
			log: newTestLogger(t),
			config: &Config{
				ServerRetry: &RetryConfig{
					InitialInterval: 10 * time.Millisecond,
					MaxInterval:     100 * time.Millisecond,
					Multiplier:      1.5,
					MaxRetry:        5,
				},
			},
			server: &serverComponent{
				cache: &samplingStrategyCache{
					strategies: make(map[string]*cacheEntry),
				},
				sampler: &remoteSampler{
					client: mockClient,
				},
			},
		}

		strategy, err := plug.getAndCacheStrategy(context.Background(), "test-service")

		assert.NoError(t, err)
		assert.NotZero(t, strategy)
		assert.Equal(t, mockSuccessStrategy, strategy)
		assert.Equal(t, 3, callCount, "Expected the client to be called 3 times (2 failures, 1 success)")
	})

	t.Run("should stop retrying if context is cancelled", func(t *testing.T) {
		mockErr := status.Error(codes.Unavailable, "server not ready")
		mockClient := &mockSamplingClient{
			GetSamplingStrategyFunc: func(ctx context.Context, in *api_v2.SamplingStrategyParameters, opts ...grpc.CallOption) (*api_v2.SamplingStrategyResponse, error) {
				return nil, mockErr
			},
		}

		plug := &jaegerRemotePlugin{
			log: newTestLogger(t),
			config: &Config{
				ServerRetry: &RetryConfig{
					InitialInterval: 50 * time.Millisecond,
					MaxInterval:     200 * time.Millisecond,
					Multiplier:      1.5,
					MaxRetry:        10,
				},
			},
			server: &serverComponent{
				cache: &samplingStrategyCache{
					strategies: make(map[string]*cacheEntry),
				},
				sampler: &remoteSampler{
					client: mockClient,
				},
			},
		}

		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		_, err := plug.getAndCacheStrategy(ctx, "test-service")

		assert.Error(t, err)
	})
}

func Test_InitServer_Failure(t *testing.T) {
	t.Run("fails if both http and grpc listen addresses are missing", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                 "server",
				"server.endpoint":      "dummy:1234",
				"server.service_names": "test-service",
			},
		}
		plug := &jaegerRemotePlugin{}

		err := plug.Init(ctx, fbit)
		defer func() {
			// Ensure gRPC client connection is closed to clean up background goroutines
			if plug.server != nil && plug.server.sampler != nil && plug.server.sampler.conn != nil {
				plug.server.sampler.conn.Close()
			}
		}()

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to initialize server mode: ")
	})
}

func Test_corsMiddleware(t *testing.T) {
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("OK"))
		assert.NoError(t, err)
	})

	plug := &jaegerRemotePlugin{
		log: newTestLogger(t),
		config: &Config{
			ServerCors: CorsSettings{
				AllowedOrigins: []string{"http://localhost:3000"},
			},
		},
	}
	testHandler := plug.corsMiddleware(dummyHandler) //

	t.Run("GET request from allowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rr := httptest.NewRecorder()

		testHandler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "http://localhost:3000", rr.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("pre-flight OPTIONS request from allowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/sampling", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rr := httptest.NewRecorder()

		testHandler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNoContent, rr.Code)
		assert.Equal(t, "http://localhost:3000", rr.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("GET request from disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling", nil)
		req.Header.Set("Origin", "https://evil-site.com")
		rr := httptest.NewRecorder()

		testHandler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "", rr.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("pre-flight OPTIONS request from disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/sampling", nil)
		req.Header.Set("Origin", "https://evil-site.com")
		rr := httptest.NewRecorder()

		testHandler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("request without CORS config should pass through", func(t *testing.T) {
		plugWithoutCors := &jaegerRemotePlugin{
			log:    newTestLogger(t),
			config: &Config{},
		}
		handlerWithoutCors := plugWithoutCors.corsMiddleware(dummyHandler)

		req := httptest.NewRequest(http.MethodGet, "/sampling", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		rr := httptest.NewRecorder()

		handlerWithoutCors.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "", rr.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("wildcard origin allows any origin", func(t *testing.T) {
		plugWithWildcard := &jaegerRemotePlugin{
			log: newTestLogger(t),
			config: &Config{
				ServerCors: CorsSettings{
					AllowedOrigins: []string{"*"},
				},
			},
		}
		handlerWithWildcard := plugWithWildcard.corsMiddleware(dummyHandler)

		req := httptest.NewRequest(http.MethodGet, "/sampling", nil)
		req.Header.Set("Origin", "https://any-site.com")
		rr := httptest.NewRecorder()

		handlerWithWildcard.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "https://any-site.com", rr.Header().Get("Access-Control-Allow-Origin"))
	})
}

func Test_ServerHandlers(t *testing.T) {
	mockJaeger := &samplingServer{
		err: status.Error(codes.NotFound, "strategy not found for service"),
	}
	upstreamJaegerServer, lis := startMockGrpcServer(t, mockJaeger)
	defer upstreamJaegerServer.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	assert.NoError(t, err)
	defer conn.Close()
	mockSampler := &remoteSampler{
		client: api_v2.NewSamplingManagerClient(conn),
		conn:   conn,
	}

	plug := &jaegerRemotePlugin{
		log:    newTestLogger(t),
		config: &Config{ServerReloadInterval: 5 * time.Minute},
		server: &serverComponent{
			cache: &samplingStrategyCache{
				strategies: make(map[string]*cacheEntry),
			},
			sampler: mockSampler,
		},
	}

	testStrategy := &api_v2.SamplingStrategyResponse{
		StrategyType:          api_v2.SamplingStrategyType_PROBABILISTIC,
		ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{SamplingRate: 0.99},
	}
	plug.server.cache.strategies["test-service"] = &cacheEntry{
		strategy:   testStrategy,
		expires_at: time.Now().Add(1 * time.Minute),
	}

	t.Run("HTTP handler returns correct strategy for existing service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling?service=test-service", nil)
		rr := httptest.NewRecorder()

		s := &grpcApiServer{plug: plug}
		s.plug.handleSampling(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var resp api_v2.SamplingStrategyResponse
		err := json.NewDecoder(rr.Body).Decode(&resp)
		assert.NoError(t, err)
		assert.Equal(t, testStrategy.StrategyType, resp.StrategyType)
	})

	t.Run("HTTP handler returns stil 200 for non-existent service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling?service=unknown-service", nil)
		rr := httptest.NewRecorder()

		s := &grpcApiServer{plug: plug}
		s.plug.handleSampling(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("HTTP handler returns 400 Bad Request for missing service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling", nil) // No service param
		rr := httptest.NewRecorder()
		plug.handleSampling(rr, req)
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("gRPC handler returns InvalidArgument for missing service name", func(t *testing.T) {
		s := &grpcApiServer{plug: plug}
		params := &api_v2.SamplingStrategyParameters{ServiceName: ""} // Empty service name
		_, err := s.GetSamplingStrategy(context.Background(), params)
		st, ok := status.FromError(err)
		assert.True(t, ok)
		assert.Equal(t, codes.InvalidArgument, st.Code())
	})
}

func getFreePort(t *testing.T) string {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	assert.NoError(t, err, "failed to resolve free port")
	l, err := net.ListenTCP("tcp", addr)
	assert.NoError(t, err, "failed to listen on free port")
	defer l.Close()
	return l.Addr().String()
}
