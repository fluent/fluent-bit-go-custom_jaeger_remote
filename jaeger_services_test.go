package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/assert/v2"
	"github.com/calyptia/plugin"
	api_v2 "github.com/jaegertracing/jaeger-idl/proto-gen/api_v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type observedCall struct {
	Context context.Context
	Params  *api_v2.SamplingStrategyParameters
}

type samplingServer struct {
	api_v2.UnimplementedSamplingManagerServer
	mu            sync.Mutex
	observedCalls []observedCall
	strategy      *api_v2.SamplingStrategyResponse
}

func (s *samplingServer) GetSamplingStrategy(ctx context.Context, params *api_v2.SamplingStrategyParameters) (*api_v2.SamplingStrategyResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observedCalls = append(s.observedCalls, observedCall{Context: ctx, Params: params})
	return s.strategy, nil
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// CORRECTED: Use integer values for strategyType to match the Go struct definition.
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

		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.strategy_file":    tmpFile.Name(),
				"server.http.listen_addr": getFreePort(t),
			},
		}
		plug := &jaegerRemotePlugin{}
		err = plug.Init(ctx, fbit)
		assert.NoError(t, err)
		assert.NotZero(t, plug.server)
		assert.NotZero(t, plug.server.httpServer)
		defer plug.server.httpServer.Close()

		assert.NotZero(t, plug.server.cache)
		assert.Equal(t, 2, len(plug.server.cache.strategies))

		// Verify HTTP endpoint serves the file content
		req := httptest.NewRequest(http.MethodGet, "/sampling?service=service-a", nil)
		rr := httptest.NewRecorder()
		plug.handleSampling(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)

		var resp api_v2.SamplingStrategyResponse
		err = json.NewDecoder(rr.Body).Decode(&resp)
		assert.NoError(t, err)
		assert.Equal(t, 0.5, resp.ProbabilisticSampling.GetSamplingRate())
	})
}

func Test_InitClient(t *testing.T) {
	t.Run("successfully initializes in client mode", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

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
		plug := &jaegerRemotePlugin{}

		err := plug.Init(ctx, fbit)

		assert.NoError(t, err)
		assert.NotZero(t, plug.clientTracer, "clientTracer should be initialized")
		assert.NotZero(t, plug.clientTracer.tracerProvider, "tracerProvider should be initialized")
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			mockStrategy := &api_v2.SamplingStrategyResponse{StrategyType: api_v2.SamplingStrategyType_RATE_LIMITING, RateLimitingSampling: &api_v2.RateLimitingSamplingStrategy{MaxTracesPerSecond: 100}}
			mockJaeger := &samplingServer{strategy: mockStrategy}
			upstreamJaegerServer, lis := startMockGrpcServer(t, mockJaeger)
			defer upstreamJaegerServer.Stop()

			fbit := &plugin.Fluentbit{
				Logger: newTestLogger(t),
				Conf: mapConfigLoader{
					"mode":                          "server",
					"server.endpoint":               "bufnet",
					"server.http.listen_addr":       getFreePort(t),
					"server.service_names":          "test-service",
					"server.retry.initial_interval": "10ms",
					"server.headers":                tc.configHeaders,
				},
			}
			plug := &jaegerRemotePlugin{}

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
				conn, err := grpc.DialContext(ctx, cfg.ServerEndpoint, dialOpts...)
				if err != nil {
					return nil, err
				}
				client := api_v2.NewSamplingManagerClient(conn)
				return &remoteSampler{conn: conn, client: client}, nil
			}

			err := plug.Init(ctx, fbit)
			assert.NoError(t, err)
			assert.NotZero(t, plug.server)
			if plug.server.httpServer != nil {
				defer plug.server.httpServer.Close()
			}

			var success bool
			for i := 0; i < 20; i++ { // Poll for up to 200ms
				if mockJaeger.callCount() >= 1 {
					success = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			assert.True(t, success, "poller should have called GetSamplingStrategy at least once")

			lastCall, ok := mockJaeger.lastCall()
			assert.True(t, ok, "expected at least one call to have been observed")
			assert.Equal(t, "test-service", lastCall.Params.ServiceName)

			md, ok := metadata.FromIncomingContext(lastCall.Context)
			assert.True(t, ok)
			for k, v := range tc.expectedHeaders {
				assert.Equal(t, []string{v}, md.Get(k))
			}
		})
	}
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

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "neither 'server.http.listen_addr' nor 'server.grpc.listen_addr' are configured")
	})
}

func Test_ServerHandlers(t *testing.T) {
	plug := &jaegerRemotePlugin{
		log: newTestLogger(t),
		server: &serverComponent{
			cache: &samplingStrategyCache{
				strategies: make(map[string]*api_v2.SamplingStrategyResponse),
			},
		},
	}

	// Manually populate the cache for the test
	testStrategy := &api_v2.SamplingStrategyResponse{
		StrategyType:          api_v2.SamplingStrategyType_PROBABILISTIC,
		ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{SamplingRate: 0.99},
	}
	plug.server.cache.strategies["test-service"] = testStrategy

	t.Run("HTTP handler returns correct strategy for existing service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling?service=test-service", nil)
		rr := httptest.NewRecorder()

		plug.handleSampling(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var resp api_v2.SamplingStrategyResponse
		err := json.NewDecoder(rr.Body).Decode(&resp)
		assert.NoError(t, err)
		assert.Equal(t, testStrategy.StrategyType, resp.StrategyType)
	})

	t.Run("HTTP handler returns 404 for non-existent service", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sampling?service=unknown-service", nil)
		rr := httptest.NewRecorder()

		plug.handleSampling(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code)
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

func mapToString(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ",")
}
