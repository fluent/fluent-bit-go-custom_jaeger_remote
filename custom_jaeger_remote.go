package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/calyptia/plugin"
	"go.opentelemetry.io/contrib/samplers/jaegerremote"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/trace"
)

const defaultRate = 1 * time.Second

func init() {
	plugin.RegisterCustom("jaeger_remote", "Jaeger remote sampling", &jeagerRemotePlugin{})
}

type jeagerRemotePlugin struct {
	log         plugin.Logger
	serverURL   string
	samplingURL string
	rate        time.Duration // default 1s
}

func configure(ctx context.Context, fbit *plugin.Fluentbit, plug *jeagerRemotePlugin) error {
	plug.log = fbit.Logger
	plug.serverURL = fbit.Conf.String("server_url")
	plug.samplingURL = fbit.Conf.String("sampling_url")
	plug.log.Debug("[jaeger_remote] server_url = '%s'", plug.serverURL)
	plug.log.Debug("[jaeger_remote] sampling_url = '%s'", plug.samplingURL)

	if plug.serverURL == "" {
		return errors.New("jarger_remote: server_url must be set")
	}

	if plug.samplingURL == "" {
		return errors.New("jarger_remote: sampling_url must be set")
	}

	if s := fbit.Conf.String("rate"); s != "" {
		var err error
		plug.rate, err = time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("jarger_remote: parse rate as duration: %w", err)
		}

		if plug.rate < 0 {
			return errors.New("jarger_remote: rate must be a positive duration")
		}

		if plug.rate == 0 {
			plug.log.Info("jarger_remote: rate set to 0, using default rate %s", defaultRate)
		}
	} else {
		plug.log.Info("jarger_remote: rate not set, using default rate %s", defaultRate)
	}

	if plug.rate == 0 {
		plug.rate = defaultRate
	}

	return nil
}

func newSampler(plug *jeagerRemotePlugin) *jaegerremote.Sampler {
	return jaegerremote.New(
		"fluent-bit-go",
		jaegerremote.WithSamplingServerURL(plug.samplingURL),
		jaegerremote.WithSamplingRefreshInterval(10*time.Second), // decrease polling interval to get quicker feedback
		jaegerremote.WithInitialSampler(trace.TraceIDRatioBased(0.5)),
	)
}

func newTraceProvider(
	ctx context.Context,
	plug *jeagerRemotePlugin,
	sampler *jaegerremote.Sampler,
) (*trace.TracerProvider, error) {
	client := otlptracehttp.NewClient(
		otlptracehttp.WithEndpoint(plug.serverURL),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
	)
	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, err
	}

	tp := trace.NewTracerProvider(
		trace.WithSampler(sampler),
		trace.WithSyncer(exporter), // for production usage, use trace.WithBatcher(exporter)
	)
	otel.SetTracerProvider(tp)

	return tp, nil
}

func (plug *jeagerRemotePlugin) Init(ctx context.Context, fbit *plugin.Fluentbit) error {
	err := configure(ctx, fbit, plug)
	if err != nil {
		return err
	}

	jaegerRemoteSampler := newSampler(plug)
	tp, err := newTraceProvider(ctx, plug, jaegerRemoteSampler)
	if err != nil {
		return err
	}

	go func() {
		ticker := time.Tick(plug.rate)
		for {
			<-ticker
			plug.log.Debug("[jaeger_remote] jeager sampling is alive %v", time.Now())
		}
	}()

	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			panic(err)
		}
	}()

	return nil
}

func main() {
}
