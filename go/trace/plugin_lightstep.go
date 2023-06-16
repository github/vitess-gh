package trace

import (
	"context"
	"fmt"
	"io"

	lstracer "github.com/lightstep/lightstep-tracer-go"
	"github.com/opentracing/opentracing-go"
	"github.com/spf13/pflag"
)

var (
	lightstepCollectorHost string
	lightstepCollectorPort int
	lightstepAccessToken   string
)

func init() {
	// If compiled with plugin_lightstep, ensure that trace.RegisterFlags
	// includes lightstep tracing flags.
	pluginFlags = append(pluginFlags, func(fs *pflag.FlagSet) {
		fs.StringVar(&lightstepCollectorHost, "lightstep-collector-host", "", "lightstep collector host. if empty, no tracing will be done")
		fs.IntVar(&lightstepCollectorPort, "lightstep-collector-port", 0, "lightstep collector port. if empty, no tracing will be done")
		fs.StringVar(&lightstepAccessToken, "lightstep-access-token", "", "unique API key for your LightStep project. if empty, no tracing will be done")
	})
}

func newLightstepTracer(serviceName string) (tracingService, io.Closer, error) {
	if lightstepAccessToken == "" {
		return nil, nil, fmt.Errorf("need access token to use lightstep tracing")
	}

	if lightstepCollectorHost == "" || lightstepCollectorPort == 0 {
		return nil, nil, fmt.Errorf("need collector host and port to use lightstep tracing")
	}

	opts := lstracer.Options{
		AccessToken: lightstepAccessToken,
		Collector: lstracer.Endpoint{
			Host: lightstepCollectorHost,
			Port: lightstepCollectorPort,
		},
	}

	if enableLogging {
		logHandler := func(event lstracer.Event) {
			switch event := event.(type) {
			case lstracer.ErrorEvent:
				(&traceLogger{}).Error(fmt.Sprintf("lightstep tracer error: %s", event.Err()))
			default:
				(&traceLogger{}).Infof("lightstep tracer: %s", event.String())
			}
		}

		lstracer.SetGlobalEventHandler(logHandler)
	}

	t := lstracer.NewTracer(opts)
	opentracing.SetGlobalTracer(t)

	return openTracingService{Tracer: &lightstepTracer{actual: t}}, &lightstepCloser{}, nil
}

var _ io.Closer = (*lightstepCloser)(nil)

type lightstepCloser struct{}

func (lightstepCloser) Close() error {
	lstracer.Close(context.Background(), opentracing.GlobalTracer())
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
