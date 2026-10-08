package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestUnitKubePrometheusStackComponentName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		release, component, want string
	}{
		{"iterabase", "prometheus", "iterabase-kube-prometheus-prometheus"},
		{"opo1", "prometheus", "opo1-kube-prometheus-stack-prometheus"},
		{"opo1", "alertmanager", "opo1-kube-prometheus-stack-alertmanager"},
	} {
		t.Run(test.release+"-"+test.component, func(t *testing.T) {
			t.Parallel()
			if got := kubePrometheusStackComponentNameForRelease(test.release, test.component); got != test.want {
				t.Fatalf("component name=%q want=%q", got, test.want)
			}
		})
	}
}

func assertHistoricalPrometheusSample(body []byte, intervalEnd float64, want string) error {
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode query_range: %w", err)
	}
	if payload.Status != "success" {
		return fmt.Errorf("query status=%q", payload.Status)
	}
	for _, result := range payload.Data.Result {
		for _, sample := range result.Values {
			if len(sample) != 2 {
				continue
			}
			var timestamp float64
			var value string
			if json.Unmarshal(sample[0], &timestamp) != nil || json.Unmarshal(sample[1], &value) != nil {
				continue
			}
			if timestamp <= intervalEnd && value == want {
				return nil
			}
		}
	}
	return fmt.Errorf("no value=%s sample at or before bounded interval end %.3f", want, intervalEnd)
}

type prometheusTarget struct {
	ScrapePool string            `json:"scrapePool"`
	ScrapeURL  string            `json:"scrapeUrl"`
	Health     string            `json:"health"`
	Labels     map[string]string `json:"labels"`
}

type serviceMonitorEndpointExpectation struct {
	index int
	name  string
	port  string
}

func activePrometheusTargets(body []byte) ([]prometheusTarget, error) {
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			Active []prometheusTarget `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Prometheus targets: %w", err)
	}
	if payload.Status != "success" {
		return nil, fmt.Errorf("targets status=%q", payload.Status)
	}
	return payload.Data.Active, nil
}

func assertDiscoveredTargets(body []byte, names []string, requireHTTPS bool) error {
	targets, err := activePrometheusTargets(body)
	if err != nil {
		return err
	}
	for _, name := range names {
		matched := false
		for _, target := range targets {
			identity := target.ScrapePool + " " + target.ScrapeURL + " " + target.Labels["job"] + " " + target.Labels["service"]
			if !strings.Contains(identity, name) {
				continue
			}
			matched = true
			if requireHTTPS && !strings.HasPrefix(target.ScrapeURL, "https://") {
				return fmt.Errorf("target %s is not verified HTTPS (%s)", name, identity)
			}
			if target.Health != "up" {
				return fmt.Errorf("target %s health=%s (%s)", name, target.Health, identity)
			}
		}
		if !matched {
			return fmt.Errorf("no active target matched %q", name)
		}
	}
	return nil
}

func assertServiceMonitorTargets(body []byte, namespace, monitor string, expected []serviceMonitorEndpointExpectation) error {
	targets, err := activePrometheusTargets(body)
	if err != nil {
		return err
	}
	prefix := fmt.Sprintf("serviceMonitor/%s/%s/", namespace, monitor)
	matching := make([]prometheusTarget, 0, len(expected))
	for _, target := range targets {
		if strings.HasPrefix(target.ScrapePool, prefix) {
			matching = append(matching, target)
		}
	}
	if len(matching) != len(expected) {
		return fmt.Errorf("ServiceMonitor %s has %d active targets, want exactly %d", monitor, len(matching), len(expected))
	}
	for _, endpoint := range expected {
		pool := fmt.Sprintf("%s%d", prefix, endpoint.index)
		matches := 0
		for _, target := range matching {
			if target.ScrapePool != pool {
				continue
			}
			matches++
			if target.Labels["endpoint"] != endpoint.name {
				return fmt.Errorf("ServiceMonitor %s endpoint %d label=%q, want %q", monitor, endpoint.index, target.Labels["endpoint"], endpoint.name)
			}
			scrapeURL, parseErr := url.Parse(target.ScrapeURL)
			if parseErr != nil {
				return fmt.Errorf("ServiceMonitor %s endpoint %d scrape URL: %w", monitor, endpoint.index, parseErr)
			}
			if scrapeURL.Scheme != "https" {
				return fmt.Errorf("ServiceMonitor %s endpoint %d scheme=%q, want https", monitor, endpoint.index, scrapeURL.Scheme)
			}
			if scrapeURL.Port() != endpoint.port {
				return fmt.Errorf("ServiceMonitor %s endpoint %d port=%q, want %q", monitor, endpoint.index, scrapeURL.Port(), endpoint.port)
			}
			if target.Health != "up" {
				return fmt.Errorf("ServiceMonitor %s endpoint %d health=%s", monitor, endpoint.index, target.Health)
			}
		}
		if matches != 1 {
			return fmt.Errorf("ServiceMonitor %s endpoint %d has %d active targets, want exactly 1", monitor, endpoint.index, matches)
		}
	}
	return nil
}

func TestUnitHistoricalSampleRejectsFreshReplacement(t *testing.T) {
	body := []byte(`{"status":"success","data":{"result":[{"values":[[200,"1"]]}]}}`)
	if err := assertHistoricalPrometheusSample(body, 100, "1"); err == nil {
		t.Fatal("post-interval sample incorrectly proved persistence")
	}
}

func TestUnitPrometheusNoSampleIsPendingReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(server.Close)

	ready, observation, err := observePrometheusValue(server.Client(), server.URL, "redis_up", "1")
	if err != nil {
		t.Fatalf("empty successful query should remain pending: %v", err)
	}
	if ready || observation != "query returned no sample" {
		t.Fatalf("ready=%t observation=%q", ready, observation)
	}
}

func TestUnitPrometheusMalformedSuccessFailsImmediately(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing data", body: `{"status":"success"}`},
		{name: "null data", body: `{"status":"success","data":null}`},
		{name: "missing result type", body: `{"status":"success","data":{"result":[]}}`},
		{name: "unexpected result type", body: `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
		{name: "missing result", body: `{"status":"success","data":{"resultType":"vector"}}`},
		{name: "null result", body: `{"status":"success","data":{"resultType":"vector","result":null}}`},
		{name: "non-array result", body: `{"status":"success","data":{"resultType":"vector","result":{}}}`},
		{name: "null timestamp", body: `{"status":"success","data":{"resultType":"vector","result":[{"value":[null,"1"]}]}}`},
		{name: "null value", body: `{"status":"success","data":{"resultType":"vector","result":[{"value":[123,null]}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			if _, _, err := observePrometheusValue(server.Client(), server.URL, "redis_up", "1"); err == nil {
				t.Fatal("malformed successful query was treated as pending readiness")
			}
		})
	}
}

func TestUnitPrometheusObservationErrorsFailImmediately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`not-json`))
	}))
	t.Cleanup(server.Close)

	if _, _, err := observePrometheusValue(server.Client(), server.URL, "redis_up", "1"); err == nil {
		t.Fatal("malformed Prometheus response was treated as pending readiness")
	}
}

func TestUnitServiceMonitorTargetsRequireEveryVerifiedTarget(t *testing.T) {
	serverExpected := []serviceMonitorEndpointExpectation{{index: 0, name: "http-web", port: "9090"}}
	reloaderExpected := []serviceMonitorEndpointExpectation{{index: 0, name: "reloader-web", port: "8080"}}
	valid := []byte(`{"status":"success","data":{"activeTargets":[{"scrapePool":"serviceMonitor/ns/prometheus-internal-tls/0","scrapeUrl":"https://prometheus:9090/metrics","health":"up","labels":{"endpoint":"http-web"}},{"scrapePool":"serviceMonitor/ns/prometheus-reloader-internal-tls/0","scrapeUrl":"https://prometheus-reloader:8080/metrics","health":"up","labels":{"endpoint":"reloader-web"}}]}}`)
	if err := assertServiceMonitorTargets(valid, "ns", "prometheus-internal-tls", serverExpected); err != nil {
		t.Fatalf("valid server target rejected: %v", err)
	}
	if err := assertServiceMonitorTargets(valid, "ns", "prometheus-reloader-internal-tls", reloaderExpected); err != nil {
		t.Fatalf("valid reloader target rejected: %v", err)
	}
	for name, body := range map[string][]byte{
		"missing-reloader":   []byte(`{"status":"success","data":{"activeTargets":[{"scrapePool":"serviceMonitor/ns/prometheus-internal-tls/0","scrapeUrl":"https://prometheus:9090/metrics","health":"up","labels":{"endpoint":"http-web"}}]}}`),
		"plaintext-reloader": []byte(`{"status":"success","data":{"activeTargets":[{"scrapePool":"serviceMonitor/ns/prometheus-internal-tls/0","scrapeUrl":"https://prometheus:9090/metrics","health":"up","labels":{"endpoint":"http-web"}},{"scrapePool":"serviceMonitor/ns/prometheus-reloader-internal-tls/0","scrapeUrl":"http://prometheus-reloader:8080/metrics","health":"up","labels":{"endpoint":"reloader-web"}}]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := assertServiceMonitorTargets(body, "ns", "prometheus-reloader-internal-tls", reloaderExpected); err == nil {
				t.Fatal("missing or plaintext reloader target incorrectly passed")
			}
		})
	}
}

func TestUnitTLSTargetRejectsPlaintext(t *testing.T) {
	body := []byte(`{"status":"success","data":{"activeTargets":[{"scrapePool":"serviceMonitor/ns/prometheus/0","scrapeUrl":"http://prometheus:9090/metrics","health":"up","labels":{"job":"prometheus"}}]}}`)
	if err := assertDiscoveredTargets(body, []string{"prometheus"}, true); err == nil {
		t.Fatal("plaintext target incorrectly proved verified HTTPS")
	}
}
