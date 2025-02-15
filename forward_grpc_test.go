package veneur_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stripe/veneur/v14"
	"github.com/stripe/veneur/v14/samplers"
	"github.com/stripe/veneur/v14/sinks"
	"github.com/stripe/veneur/v14/trace"
	"github.com/stripe/veneur/v14/util"
)

type forwardGRPCFixture struct {
	t      testing.TB
	proxy  *veneur.Proxy
	global *veneur.Server
	local  *veneur.Server
}

// generateConfig is not called config to avoid
// accidental variable shadowing
func generateConfig() veneur.Config {
	return veneur.Config{
		Debug: true,

		// Use a shorter interval for tests
		Interval:            veneur.DefaultFlushInterval,
		Percentiles:         []float64{.5, .75, .99},
		Aggregates:          []string{"min", "max", "count"},
		ReadBufferSizeBytes: 2097152,
		HTTPAddress:         "localhost:0",
		NumWorkers:          4,

		// Use only one reader, so that we can run tests
		// on platforms which do not support SO_REUSEPORT
		NumReaders: 1,

		// Currently this points nowhere, which is intentional.
		// We don't need internal metrics for the tests, and they make testing
		// more complicated.
		StatsAddress: "localhost:8125",

		// Don't use the default port 8128: Veneur sends its own traces there, causing failures
		SsfListenAddresses: []util.Url{{
			Value: &url.URL{
				Scheme: "udp",
				Host:   "127.0.0.1:0",
			},
		}},
		TraceMaxLengthBytes: 4096,
	}
}

// setupVeneurServer creates a local server from the specified config
// and starts listening for requests. It returns the server for
// inspection.  If no metricSink or spanSink are provided then a
// `black hole` sink will be used so that flushes to these sinks do
// "nothing".
func setupVeneurServer(
	t testing.TB, config veneur.Config,
) *veneur.Server {
	logger := logrus.New()
	server, err := veneur.NewFromConfig(veneur.ServerConfig{
		Logger: logger,
		Config: config,
		MetricSinkTypes: veneur.MetricSinkTypes{
			"channel": {
				Create: func(
					server *veneur.Server, name string, logger *logrus.Entry,
					config veneur.Config, sinkConfig veneur.MetricSinkConfig,
				) (sinks.MetricSink, error) {
					ch, ok := sinkConfig.(chan []samplers.InterMetric)
					if !ok {
						return nil, errors.New("invalid config")
					}
					return veneur.NewChannelMetricSink(ch)
				},
				ParseConfig: func(
					name string, config interface{},
				) (veneur.MetricSinkConfig, error) {
					return config, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Make sure we don't send internal metrics when testing:
	trace.NeutralizeClient(server.TraceClient)

	server.Start()
	return server
}

type testHTTPStarter interface {
	IsListeningHTTP() bool
}

// waitForHTTPStart blocks until the Server's HTTP server is started, or until
// the specified duration is elapsed.
func waitForHTTPStart(t testing.TB, s testHTTPStarter, timeout time.Duration) {
	tickCh := time.Tick(10 * time.Millisecond)
	timeoutCh := time.After(timeout)

	for {
		select {
		case <-tickCh:
			if s.IsListeningHTTP() {
				return
			}
		case <-timeoutCh:
			t.Errorf("The HTTP server did not start within the specified duration")
		}
	}
}

// newForwardGRPCFixture creates a set of resources that forward to each other
// over gRPC.  Specifically this includes a local Server, which forwards
// metrics over gRPC to a Proxy, which then forwards over gRPC again to a
// global Server.
func newForwardGRPCFixture(
	t testing.TB, ch chan []samplers.InterMetric,
) *forwardGRPCFixture {
	// Create a global Veneur
	globalConfig := generateConfig()
	globalConfig.GrpcAddress = unusedLocalTCPAddress(t)
	globalConfig.MetricSinks = []veneur.SinkConfig{{
		Name:   "channel",
		Kind:   "channel",
		Config: ch,
	}}
	global := setupVeneurServer(t, globalConfig)
	go func() {
		global.Serve()
	}()
	waitForHTTPStart(t, global, 3*time.Second)

	// Create a proxy Veneur
	proxyConfig := veneur.ProxyConfig{
		Debug:                  false,
		ConsulRefreshInterval:  "86400s",
		ConsulTraceServiceName: "traceServiceName",
		TraceAddress:           "127.0.0.1:8128",
		TraceAPIAddress:        "127.0.0.1:8135",
		HTTPAddress:            "127.0.0.1:0",
		GrpcAddress:            unusedLocalTCPAddress(t),
		StatsAddress:           "127.0.0.1:8201",
		GrpcForwardAddress:     globalConfig.GrpcAddress,
	}
	proxy, err := veneur.NewProxyFromConfig(logrus.New(), proxyConfig)
	assert.NoError(t, err)
	go func() {
		proxy.Serve()
	}()
	waitForHTTPStart(t, proxy, 3*time.Second)

	localConfig := generateConfig()
	localConfig.ForwardAddress = proxyConfig.GrpcAddress
	localConfig.ForwardUseGrpc = true
	local := setupVeneurServer(t, localConfig)

	return &forwardGRPCFixture{t: t, proxy: proxy, global: global, local: local}
}

// stop stops all of the various servers inside the fixture.
func (ff *forwardGRPCFixture) stop() {
	ff.proxy.Shutdown()
	ff.global.Shutdown()
	ff.local.Shutdown()
}

// IngestMetric synchronously writes a metric to the forwarding
// fixture's local veneur span worker. The fixture must be flushed via
// the (*forwardFixture).local.Flush method so the ingestion effects can be
// observed.
func (ff *forwardGRPCFixture) IngestMetric(m *samplers.UDPMetric) {
	ff.local.Workers[0].ProcessMetric(m)
}

// unusedLocalTCPAddress returns a host:port combination on the loopback
// interface that *should* be available for usage.

// This is definitely pretty hacky, but I was having a tricky time coming
// up with a good way to expose the gRPC listening address of the server or
// proxy without awkwardly saving the listener or listen address, and then
// modifying the gRPC serve function to create the network listener before
// spawning the goroutine.
func unusedLocalTCPAddress(t testing.TB) string {
	ln, err := net.Listen("tcp", "127.0.0.1:")
	if err != nil {
		t.Fatalf("Failed to bind to a test port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func forwardGRPCTestMetrics() []*samplers.UDPMetric {
	return []*samplers.UDPMetric{{
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.histogram",
			Type: veneur.HistogramTypeName,
		},
		Value:      20.0,
		Digest:     12345,
		SampleRate: 1.0,
		Scope:      samplers.MixedScope,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.histogram_global",
			Type: veneur.HistogramTypeName,
		},
		Value:      20.0,
		Digest:     12345,
		SampleRate: 1.0,
		Scope:      samplers.GlobalOnly,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.gauge",
			Type: veneur.GaugeTypeName,
		},
		Value:      1.0,
		SampleRate: 1.0,
		Scope:      samplers.GlobalOnly,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.counter",
			Type: veneur.CounterTypeName,
		},
		Value:      2.0,
		SampleRate: 1.0,
		Scope:      samplers.GlobalOnly,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.timer_mixed",
			Type: veneur.TimerTypeName,
		},
		Value:      100.0,
		Digest:     12345,
		SampleRate: 1.0,
		Scope:      samplers.MixedScope,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.timer",
			Type: veneur.TimerTypeName,
		},
		Value:      100.0,
		Digest:     12345,
		SampleRate: 1.0,
		Scope:      samplers.GlobalOnly,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.set",
			Type: veneur.SetTypeName,
		},
		Value:      "test",
		SampleRate: 1.0,
		Scope:      samplers.GlobalOnly,
	}, {
		MetricKey: samplers.MetricKey{
			Name: "test.grpc.counter.local",
			Type: veneur.CounterTypeName,
		},
		Value:      100.0,
		Digest:     12345,
		SampleRate: 1.0,
		Scope:      samplers.MixedScope,
	}}
}

// TestE2EForwardingGRPCMetrics inputs a set of metrics to a local Veneur,
// and verifies that the same metrics are later flushed by the global Veneur
// after passing through a proxy.
func TestE2EForwardingGRPCMetrics(t *testing.T) {
	ch := make(chan []samplers.InterMetric)

	ff := newForwardGRPCFixture(t, ch)
	defer ff.stop()

	input := forwardGRPCTestMetrics()
	for _, metric := range input {
		ff.IngestMetric(metric)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)

		expected := map[string]bool{}
		for _, name := range []string{
			"test.grpc.histogram.50percentile",
			"test.grpc.histogram.75percentile",
			"test.grpc.histogram.99percentile",
			"test.grpc.histogram_global.99percentile",
			"test.grpc.histogram_global.50percentile",
			"test.grpc.histogram_global.75percentile",
			"test.grpc.histogram_global.max",
			"test.grpc.histogram_global.min",
			"test.grpc.histogram_global.count",
			"test.grpc.timer_mixed.50percentile",
			"test.grpc.timer_mixed.75percentile",
			"test.grpc.timer_mixed.99percentile",
			"test.grpc.timer.50percentile",
			"test.grpc.timer.75percentile",
			"test.grpc.timer.99percentile",
			"test.grpc.timer.max",
			"test.grpc.timer.min",
			"test.grpc.timer.count",
			"test.grpc.counter",
			"test.grpc.gauge",
			"test.grpc.set",
		} {
			expected[name] = false
		}

	metrics:
		for {
			metrics := <-ch

			for _, metric := range metrics {
				_, ok := expected[metric.Name]
				if !ok {
					t.Errorf("unexpected metric %q", metric.Name)
					continue
				}
				expected[metric.Name] = true
			}
			for name, got := range expected {
				if !got {
					// we have more metrics to read:
					t.Logf("metric %q still missing", name)
					continue metrics
				}
			}
			// if there had been metrics to read, we'd
			// have restarted the loop:
			return
		}

	}()
	ff.local.Flush(context.TODO())
	ff.global.Flush(context.TODO())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for a metric after 3 seconds")
	}
}
