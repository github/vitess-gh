package trace

import (
	"context"
	"fmt"
	"io"

	lstracer "github.com/lightstep/lightstep-tracer-go"
	"github.com/opentracing/opentracing-go"
	"github.com/spf13/pflag"

	"vitess.io/vitess/go/viperutil"
)

var (
	lightstepConfigKey = viperutil.KeyPrefixFunc(configKey("lightstep"))

	lightstepCollectorHost = viperutil.Configure(
		lightstepConfigKey("collector.host"),
		viperutil.Options[string]{
			FlagName: "lightstep-collector-host",
		},
	)
	lightstepCollectorPort = viperutil.Configure(
		lightstepConfigKey("collector.port"),
		viperutil.Options[int]{
			FlagName: "lightstep-collector-port",
		},
	)
	lightstepAccessToken = viperutil.Configure(
		lightstepConfigKey("access-token"),
		viperutil.Options[string]{
			FlagName: "lightstep-access-token",
		},
	)
)

func init() {
	// If compiled with plugin_lightstep, ensure that trace.RegisterFlags
	// includes lightstep tracing flags.
	pluginFlags = append(pluginFlags, func(fs *pflag.FlagSet) {
		fs.String("lightstep-collector-host", "", "host to send spans to. if empty, no tracing will be done")
		fs.String("lightstep-collector-port", "", "port to send spans to. if empty, no tracing will be done")
		fs.String("lightstep-access-token", "", "unique API key for your LightStep project. if empty, no tracing will be done")

		viperutil.BindFlags(fs, lightstepCollectorHost, lightstepCollectorPort, lightstepAccessToken)
	})
}

func newLightstepTracer(serviceName string) (tracingService, io.Closer, error) {
	accessToken := lightstepAccessToken.Get()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("need access token to use lightstep tracing")
	}

	collectorHost, collectorPort := lightstepCollectorHost.Get(), lightstepCollectorPort.Get()
	if collectorHost == "" || collectorPort == 0 {
		return nil, nil, fmt.Errorf("need collector host and port to use lightstep tracing")
	}

	t := lstracer.NewTracer(lstracer.Options{
		AccessToken: accessToken,
		Collector: lstracer.Endpoint{
			Host: collectorHost,
			Port: collectorPort,
		},
		// TODO: sampling rate?
	})

	// TODO: logging?

	opentracing.SetGlobalTracer(t)

	return openTracingService{Tracer: &lightstepTracer{actual: t}}, &lightstepCloser{}, nil
}

var _ io.Closer = (*lightstepCloser)(nil)

type lightstepCloser struct{}

func (lightstepCloser) Close() error {
	lstracer.Flush(context.Background(), opentracing.GlobalTracer())
	return nil
}

func init() {
	tracingBackendFactories["opentracing-lightstep"] = newLightstepTracer
}

var _ tracer = (*lightstepTracer)(nil)

type lightstepTracer struct {
	actual opentracing.Tracer
}

func (dt *lightstepTracer) GetOpenTracingTracer() opentracing.Tracer {
	return dt.actual
}