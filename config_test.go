package main

import (
	"testing"
	"time"

	"github.com/alecthomas/assert/v2"
	"github.com/calyptia/plugin"
)

// mapConfigLoader is a mock implementation of plugin.ConfigLoader for testing.
type mapConfigLoader map[string]string

func (m mapConfigLoader) String(key string) string {
	return m[key]
}

// newTestLogger creates a logger that integrates with the Go testing framework.
func newTestLogger(t testing.TB) plugin.Logger {
	t.Helper()
	return &testLogger{t: t}
}

type testLogger struct {
	t testing.TB
}

func (l *testLogger) Error(format string, args ...any) { l.t.Logf("ERROR: "+format, args...) }
func (l *testLogger) Warn(format string, args ...any)  { l.t.Logf("WARN: "+format, args...) }
func (l *testLogger) Info(format string, args ...any)  { l.t.Logf("INFO: "+format, args...) }
func (l *testLogger) Debug(format string, args ...any) { l.t.Logf("DEBUG: "+format, args...) }

// --- Test Cases ---

func Test_loadConfig_ModeAll_Valid(t *testing.T) {
	t.Run("mode=all with all required fields should succeed", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "all",
				"client.server_url":       "http://otel-collector:4318",
				"client.sampling_url":     "http://jaeger-collector:5778/sampling",
				"server.endpoint":         "jaeger-collector:14250",
				"server.http.listen_addr": "0.0.0.0:8899",
				"server.grpc.listen_addr": "0.0.0.0:9099",
				"server.service_names":    "service1,service2",
			},
		}

		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.NotZero(t, cfg)
		assert.Equal(t, "all", cfg.Mode)
		assert.Equal(t, "http://otel-collector:4318", cfg.ClientServerURL)
		assert.Equal(t, "0.0.0.0:8899", cfg.ServerHttpListenAddr)
	})
}

func Test_loadConfig_ModeClient(t *testing.T) {
	t.Run("mode=client with required fields should succeed", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                "client",
				"client.server_url":   "http://localhost:4318",
				"client.sampling_url": "http://localhost:5778/sampling",
			},
		}
		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.Equal(t, "client", cfg.Mode)
	})

	t.Run("mode=client missing server_url should fail", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                "client",
				"client.sampling_url": "http://localhost:5778/sampling",
			},
		}
		_, err := loadConfig(fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "'client.server_url' is required")
	})

	t.Run("mode=client missing sampling_url should fail", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":              "client",
				"client.server_url": "http://localhost:4318",
			},
		}
		_, err := loadConfig(fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "'client.sampling_url' is required")
	})
}

func Test_loadConfig_ModeServer(t *testing.T) {
	t.Run("mode=server with required fields should succeed", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.endpoint":         "jaeger-collector:14250",
				"server.http.listen_addr": "0.0.0.0:8899",
				"server.service_names":    "service1",
			},
		}
		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.Equal(t, "server", cfg.Mode)
	})

	t.Run("mode=server missing endpoint should fail", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.http.listen_addr": "0.0.0.0:8899",
				"server.service_names":    "service1",
			},
		}
		_, err := loadConfig(fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "'server.endpoint' is required")
	})
}

func Test_loadConfig_Server_Defaults(t *testing.T) {
	t.Run("server mode applies defaults for retry and keepalive", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.endpoint":         "jaeger-collector:14250",
				"server.http.listen_addr": "0.0.0.0:8899",
				"server.service_names":    "service1",
				// Keepalive and Retry are omitted to test defaults
			},
		}

		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.NotZero(t, cfg.ServerRetry)
		assert.Equal(t, 5*time.Second, cfg.ServerRetry.InitialInterval)
		assert.Equal(t, 5*time.Minute, cfg.ServerRetry.MaxInterval)
		assert.Equal(t, 1.5, cfg.ServerRetry.Multiplier)
		// Keepalive should be nil as it's only configured if 'server.keepalive.time' is set
		assert.Zero(t, cfg.ServerKeepalive)
	})

	t.Run("server mode with keepalive time set", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.endpoint":         "jaeger-collector:14250",
				"server.http.listen_addr": "0.0.0.0:8899",
				"server.service_names":    "service1",
				"server.keepalive.time":   "30s",
			},
		}

		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.NotZero(t, cfg.ServerKeepalive)
		assert.Equal(t, 30*time.Second, cfg.ServerKeepalive.Time)
		// Test default timeout when time is set but timeout is not
		assert.Equal(t, 20*time.Second, cfg.ServerKeepalive.Timeout)
	})

	t.Run("server mode with invalid multiplier", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.endpoint":         "jaeger-collector:14250",
				"server.http.listen_addr": "0.0.0.0:8899",
				"server.service_names":    "service1",
				"server.retry.multiplier": "0.9", // Invalid value
			},
		}

		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.NotZero(t, cfg.ServerRetry)
		// Should be reset to the default
		assert.Equal(t, 1.5, cfg.ServerRetry.Multiplier)
	})
}
