package main

import (
	"context"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/calyptia/plugin"
)

func Test_jaegerRemote(t *testing.T) {
	ctx := context.Background()
	fbit := &plugin.Fluentbit{
		Logger: newTestLogger(t),
		Conf: mapConfigLoader{
			"server_url": "http://localhost:14268",
			"sampling_url": "http://localhost:5778/sampling",
			"rate": "5s",
		},
	}

	jr := &jeagerRemotePlugin{}
	err := jr.Init(ctx, fbit)

	assert.NoError(t, err)
}

func Test_jaegerRemoteNeededServerURL(t *testing.T) {
	ctx := context.Background()
	fbit := &plugin.Fluentbit{
		Logger: newTestLogger(t),
		Conf: mapConfigLoader{
			"server_url": "",
			"sampling_url": "http://localhost:5778/sampling",
			"rate": "15s",
		},
	}

	jr := &jeagerRemotePlugin{}
	err := jr.Init(ctx, fbit)

	assert.Error(t, err)
}

func Test_jaegerRemoteNeededSamplingURL(t *testing.T) {
	ctx := context.Background()
	fbit := &plugin.Fluentbit{
		Logger: newTestLogger(t),
		Conf: mapConfigLoader{
			"server_url": "http://localhost:14268",
			"sampling_url": "",
			"rate": "30s",
		},
	}

	jr := &jeagerRemotePlugin{}
	err := jr.Init(ctx, fbit)

	assert.Error(t, err)
}

type mapConfigLoader map[string]string

func (m mapConfigLoader) String(key string) string {
	return m[key]
}

func newTestLogger(testing testing.TB) plugin.Logger {
	testing.Helper()
	return &testLogger{t: testing}
}

type testLogger struct {
	t testing.TB
}

func (l *testLogger) Error(format string, args ...any) {
	l.t.Logf(format, args...)
}

func (l *testLogger) Warn(format string, args ...any) {
	l.t.Logf(format, args...)
}

func (l *testLogger) Info(format string, args ...any) {
	l.t.Logf(format, args...)
}

func (l *testLogger) Debug(format string, args ...any) {
	l.t.Logf(format, args...)
}

func assertNonEmptyString(testing testing.TB, x any, varName string) {
	testing.Helper()
	s, ok := x.(string)
	assert.True(testing, ok, "%q is not a string", varName)
	assert.NotZero(testing, s, "%q is empty", varName)
}

func assertFloatBetween(testing testing.TB, x any, min, max float64, varName string) {
	testing.Helper()
	f, ok := x.(float64)
	assert.True(testing, ok, "%q is not a float", varName)
	assert.True(testing, f >= min && f <= max, "%q is out of range", varName)
}
