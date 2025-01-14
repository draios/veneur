package consul_test

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stripe/veneur/v14"
)

type ConsulOneRoundTripper struct {
	HealthGotCalled bool
}

func generateProxyConfig() veneur.ProxyConfig {
	return veneur.ProxyConfig{
		Debug:                    false,
		ConsulRefreshInterval:    "86400s",
		ConsulForwardServiceName: "forwardServiceName",
		ConsulTraceServiceName:   "traceServiceName",
		TraceAddress:             "127.0.0.1:8128",
		TraceAPIAddress:          "127.0.0.1:8135",
		HTTPAddress:              "127.0.0.1:0",
		GrpcAddress:              "127.0.0.1:0",
		StatsAddress:             "127.0.0.1:8201",
	}
}

func (rt *ConsulOneRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	if strings.HasPrefix(req.URL.Path, "/v1/health/service/") {
		resp, err := ioutil.ReadFile("testdata/health_service_one.json")
		if err != nil {
			return nil, err
		}
		rec.Write(resp)
		rec.Code = http.StatusOK
		rt.HealthGotCalled = true
	}

	return rec.Result(), nil
}

type ConsulChangingRoundTripper struct {
	Count           int
	HealthGotCalled bool
}

func (rt *ConsulChangingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	if req.URL.Path == "/v1/health/service/forwardServiceName" {
		var resp []byte
		var err error
		switch rt.Count {
		case 1:
			// On the first invocation, return two hosts
			resp, err = ioutil.ReadFile("testdata/health_service_two.json")
		case 2:
			// On the second invocation, return zero hosts
			resp, err = ioutil.ReadFile("testdata/health_service_zero.json")
		default:
			resp, err = ioutil.ReadFile("testdata/health_service_one.json")
		}
		if err != nil {
			return nil, err
		}
		rec.Write(resp)
		rec.Code = http.StatusOK
		rt.HealthGotCalled = true
		rt.Count++
	} else if req.URL.Path == "/v1/health/service/traceServiceName" {
		// These don't count. Since we make different calls, we'll return some junk
		// for tracing and leave forwarding to it's own thing.
		resp, err := ioutil.ReadFile("testdata/health_service_one.json")
		if err != nil {
			return nil, err
		}
		rec.Write(resp)
		rec.Code = http.StatusOK
	}

	return rec.Result(), nil
}

func TestConsulOneHost(t *testing.T) {
	config := generateProxyConfig()
	transport := &ConsulOneRoundTripper{}
	server, _ := veneur.NewProxyFromConfig(logrus.New(), config)

	server.HTTPClient.Transport = transport

	server.Start()
	defer server.Shutdown()

	assert.True(t, transport.HealthGotCalled, "Health Service got called")
	assert.Equal(t, "10.1.10.12:8000", server.ForwardDestinations.Members()[0], "Get ring member from Consul")
	assert.Len(t, server.ForwardDestinations.Members(), 1, "Only one host in ring")
}

func TestConsulChangingHosts(t *testing.T) {
	config := generateProxyConfig()
	transport := &ConsulChangingRoundTripper{}
	server, _ := veneur.NewProxyFromConfig(logrus.New(), config)

	server.HTTPClient.Transport = transport

	server.Start()
	defer server.Shutdown()
	// First invocation is during startup
	assert.Equal(t, 1, transport.Count, "Health got called once")
	assert.Equal(t, "10.1.10.12:8000", server.ForwardDestinations.Members()[0], "Get ring member from Consul")
	assert.Len(t, server.ForwardDestinations.Members(), 1, "Only one host in ring")

	// Refresh! Should have two now
	server.RefreshDestinations(config.ConsulForwardServiceName, server.ForwardDestinations, &server.ForwardDestinationsMtx)
	assert.Equal(t, 2, transport.Count, "Health got called second time")
	assert.Contains(t, server.ForwardDestinations.Members(), "10.1.10.12:8000", "Got first member from Consul")
	assert.Contains(t, server.ForwardDestinations.Members(), "10.1.10.13:8000", "Got second member from Consul")
	assert.Len(t, server.ForwardDestinations.Members(), 2, "Two hosts host in ring")

	// Refresh! Now just none!
	server.RefreshDestinations(config.ConsulForwardServiceName, server.ForwardDestinations, &server.ForwardDestinationsMtx)
	assert.Equal(t, 3, transport.Count, "Health got called a third time.")
	assert.Contains(t, server.ForwardDestinations.Members(), "10.1.10.12:8000", "Got first member from Consul")
	assert.Contains(t, server.ForwardDestinations.Members(), "10.1.10.13:8000", "Got second member from Consul")
	assert.Len(t, server.ForwardDestinations.Members(), 2, "Two hosts host in ring")

	// Refresh! Now just one!
	server.RefreshDestinations(config.ConsulForwardServiceName, server.ForwardDestinations, &server.ForwardDestinationsMtx)
	assert.Equal(t, 4, transport.Count, "Health got called fourth time")
	assert.Contains(t, server.ForwardDestinations.Members(), "10.1.10.12:8000", "Got first member from Consul")
	assert.Len(t, server.ForwardDestinations.Members(), 1, "One host host in ring")
}
