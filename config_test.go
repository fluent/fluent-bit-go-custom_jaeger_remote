package main

import (
	"os"
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
		assert.Equal(t, 5 *time.Second, cfg.ClientRate)
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

	t.Run("client mode with negative client.rate should fail", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                   "client",
				"client.server_url": "http://localhost:4318",
				"client.sampling_url": "http://localhost:5778/sampling",
				"client.rate": "-20s", // Invalid format
			},
		}
		_, err := loadConfig(fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rate must be a positive duration")
	})

	t.Run("client mode with not to be succeeded to parse client.rate should fail", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                   "client",
				"client.server_url": "http://localhost:4318",
				"client.sampling_url": "http://localhost:5778/sampling",
				"client.rate": "-20", // Invalid format
			},
		}
		_, err := loadConfig(fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "cannot parse rate as duration: time: missing unit in duration ")
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
		assert.Contains(t, err.Error(), "for server mode, either 'server.endpoint' or 'server.strategy_file' must be configured")
	})
}

func Test_loadConfig_InvalidValues(t *testing.T) {
    // Test for a bad duration value
	t.Run("server mode with invalid reload_interval logs warning and uses default", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                    "server",
				"server.endpoint":         "jaeger-collector:14250",
				"server.service_names":    "service1",
				"server.reload_interval":  "5minutes", // Invalid format
			},
		}
		cfg, err := loadConfig(fbit)
		assert.NoError(t, err)
		assert.Equal(t, 5*time.Minute, cfg.ServerReloadInterval)
	})

	t.Run("server mode with both endpoint and strategy_file should fail", func(t *testing.T) {
		fbit := &plugin.Fluentbit{
			Logger: newTestLogger(t),
			Conf: mapConfigLoader{
				"mode":                 "server",
				"server.endpoint":      "jaeger-collector:14250",
				"server.strategy_file": "/path/to/file.json",
			},
		}
		_, err := loadConfig(fbit)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "are mutually exclusive")
	})
}

func Test_loadTLSConfig_FileErrors(t *testing.T) {
	t.Run("should fail with non-existent CA file", func(t *testing.T) {
		cfg := TLSSettings{CAFile: "/tmp/this-file-does-not-exist.ca"}
		_, err := loadTLSConfig(cfg)
		assert.Error(t, err)
	})

	t.Run("should fail with invalid cert/key pair", func(t *testing.T) {
		// Create dummy (but invalid) files
		certFile, _ := os.CreateTemp("", "cert")
		defer os.Remove(certFile.Name())
		keyFile, _ := os.CreateTemp("", "key")
		defer os.Remove(keyFile.Name())

		certFile.WriteString("not a cert")
		keyFile.WriteString("not a key")

		cfg := TLSSettings{CertFile: certFile.Name(), KeyFile: keyFile.Name()}
		_, err := loadTLSConfig(cfg)
		assert.Error(t, err)
	})
}

func Test_loadConfig_CORS(t *testing.T) {
	testCases := []struct {
		name            string
		inputConf       map[string]string
		expectedOrigins []string
	}{
		{
			name: "multiple origins with spaces",
			inputConf: map[string]string{
				"server.cors.allowed_origins": "http://localhost:3000, https://my-app.com",
			},
			expectedOrigins: []string{"http://localhost:3000", "https://my-app.com"},
		},
		{
			name: "single origin",
			inputConf: map[string]string{
				"server.cors.allowed_origins": "https://my-app.com",
			},
			expectedOrigins: []string{"https://my-app.com"},
		},
		{
			name: "wildcard origin",
			inputConf: map[string]string{
				"server.cors.allowed_origins": "*",
			},
			expectedOrigins: []string{"*"},
		},
		{
			name:            "config key not present",
			inputConf:       map[string]string{}, // The key is missing entirely
			expectedOrigins: nil,                 // Expect a nil slice, not an empty one
		},
		{
			name: "config key is an empty string",
			inputConf: map[string]string{
				"server.cors.allowed_origins": "",
			},
			expectedOrigins: nil,
		},
		{
			name: "malformed string with extra spaces and commas",
			inputConf: map[string]string{
				"server.cors.allowed_origins": "  http://a.com, ,https://b.com  ,",
			},
			expectedOrigins: []string{"http://a.com", "https://b.com"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			conf := mapConfigLoader(tc.inputConf)

			parseCorsConfig(cfg, conf)

			assert.Equal(t, tc.expectedOrigins, cfg.ServerCors.AllowedOrigins)
		})
	}
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
