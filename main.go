package xk6_otel_instrumentation

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/sobek"
	"go.k6.io/k6/v2/js/modules"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type contextKey string
const customAttributesKey contextKey = "custom_span_attributes"

func init() {
	modules.Register("k6/x/otel-instrumentation", New())
}

type RootModule struct{}

func New() *RootModule {
	return &RootModule{}
}

func (r *RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	return &OtelModule{vu: vu}
}

type OtelModule struct {
	vu modules.VU
}

func (m *OtelModule) Exports() modules.Exports {
	return modules.Exports{Default: m}
}

type ClientConfig struct {
	Endpoint           string                 `js:"endpoint"`
	Insecure           bool                   `js:"insecure"`
	Sampler            string                 `js:"sampler"`
	SamplingRatio      float64                `js:"samplingRatio"`
	ResourceAttributes map[string]interface{} `js:"resourceAttributes"`
	LegacySemconv      bool                   `js:"legacySemconv"`
}

type customTransport struct {
	underlying    http.RoundTripper
	legacySemconv bool
}

func isKnownMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE", "QUERY":
		return true
	}
	return false
}

func (c *customTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	span := trace.SpanFromContext(req.Context())
	
	if span.IsRecording() {
		fullURL := req.URL.String()
		scheme := req.URL.Scheme
		if scheme == "" {
			scheme = "http"
		}
		
		path := req.URL.Path
		if path == "" {
			path = "/"
		}
		
		host := req.URL.Host
		if host == "" {
			host = req.Host
		}

		userAgent := req.UserAgent()
		if userAgent == "" {
			userAgent = "Go-http-client/1.1"
		}

		method := req.Method
		methodAttr := method
		methodOriginalAttr := ""
		if !isKnownMethod(method) {
			methodAttr = "_OTHER"
			methodOriginalAttr = method
			method = "HTTP"
		}

		urlTemplate := ""
		if customAttrs, ok := req.Context().Value(customAttributesKey).(map[string]interface{}); ok {
			if ut, exists := customAttrs["url.template"]; exists {
				if utStr, ok := ut.(string); ok {
					urlTemplate = utStr
				}
			}
		}

		spanName := method
		if urlTemplate != "" {
			spanName = fmt.Sprintf("%s %s", method, urlTemplate)
		}
		span.SetName(spanName)

		span.SetAttributes(
			attribute.String("http.request.method", methodAttr),
			attribute.String("url.full", fullURL),
			attribute.String("url.path", path),
			attribute.String("url.scheme", scheme),
			attribute.String("server.address", req.URL.Hostname()),
			attribute.String("user_agent.original", userAgent),
		)

		if methodOriginalAttr != "" {
			span.SetAttributes(attribute.String("http.request.method_original", methodOriginalAttr))
		}

		if urlTemplate != "" {
			span.SetAttributes(attribute.String("url.template", urlTemplate))
		}

		if c.legacySemconv {
			span.SetAttributes(
				attribute.String("http.method", methodAttr),
				attribute.String("http.url", fullURL),
				attribute.String("http.scheme", scheme),
				attribute.String("http.host", host),
				attribute.String("http.user_agent", userAgent),
				attribute.String("net.peer.name", req.URL.Hostname()),
			)
		}

		portStr := req.URL.Port()
		if portStr != "" {
			var port int
			_, _ = fmt.Sscanf(portStr, "%d", &port)
			if port > 0 {
				span.SetAttributes(attribute.Int64("server.port", int64(port)))
				if c.legacySemconv {
					span.SetAttributes(attribute.Int64("net.peer.port", int64(port)))
				}
			}
		} else {
			defaultPort := int64(80)
			if scheme == "https" {
				defaultPort = 443
			}
			span.SetAttributes(attribute.Int64("server.port", defaultPort))
			if c.legacySemconv {
				span.SetAttributes(attribute.Int64("net.peer.port", defaultPort))
			}
		}

		if customAttrs, ok := req.Context().Value(customAttributesKey).(map[string]interface{}); ok {
			for k, v := range customAttrs {
				if k == "url.template" {
					continue
				}
				switch val := v.(type) {
				case string:
					span.SetAttributes(attribute.String(k, val))
				case bool:
					span.SetAttributes(attribute.Bool(k, val))
				case int64:
					span.SetAttributes(attribute.Int64(k, val))
				case float64:
					span.SetAttributes(attribute.Float64(k, val))
				default:
					span.SetAttributes(attribute.String(k, fmt.Sprintf("%v", val)))
				}
			}
		}

		for _, h := range []string{"Accept", "Connection", "X-SampleRatio"} {
			if val := req.Header.Get(h); val != "" {
				span.SetAttributes(attribute.String(h, val))
			}
		}
	}

	resp, err := c.underlying.RoundTrip(req)
	
	if err != nil {
		if span.IsRecording() {
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String("error.type", getErrorType(err)))
			span.RecordError(err)
		}
		return nil, err
	}

	if resp != nil && span.IsRecording() {
		statusCode := int64(resp.StatusCode)
		
		span.SetAttributes(attribute.Int64("http.response.status_code", statusCode))
		if c.legacySemconv {
			span.SetAttributes(attribute.Int64("http.status_code", statusCode))
		}

		protoVer := strings.TrimPrefix(resp.Proto, "HTTP/")
		span.SetAttributes(attribute.String("network.protocol.version", protoVer))
		if c.legacySemconv {
			span.SetAttributes(
				attribute.String("net.protocol.version", protoVer),
				attribute.String("net.app.protocol.version", protoVer),
				attribute.String("messaging.protocol_version", protoVer),
			)
		}

		if statusCode >= 400 {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
			span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%d", statusCode)))
		}
	}

	return resp, nil
}

func getErrorType(err error) string {
	if err == nil {
		return ""
	}
	errStr := err.Error()
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") {
		return "timeout"
	}
	if strings.Contains(errStr, "no such host") || strings.Contains(errStr, "UnknownHostException") {
		return "unknown_host"
	}
	if strings.Contains(errStr, "connection refused") {
		return "connection_refused"
	}
	return err.Error()
}

func (m *OtelModule) Instrument(httpObj *sobek.Object, config ClientConfig) {
	rt := m.vu.Runtime()
	ctx := context.Background()

	var attrs []attribute.KeyValue
	for k, v := range config.ResourceAttributes {
		switch val := v.(type) {
		case string:  attrs = append(attrs, attribute.String(k, val))
		case bool:    attrs = append(attrs, attribute.Bool(k, val))
		case int64:   attrs = append(attrs, attribute.Int64(k, val))
		case float64: attrs = append(attrs, attribute.Float64(k, val))
		default:      attrs = append(attrs, attribute.String(k, fmt.Sprintf("%v", val)))
		}
	}
	
	if config.ResourceAttributes["service.name"] == nil {
		attrs = append(attrs, attribute.String("service.name", "k6-agent"))
	}

	res, _ := resource.New(ctx, resource.WithAttributes(attrs...))
	exporterOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(config.Endpoint)}
	if config.Insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}
	exporter, _ := otlptracegrpc.New(ctx, exporterOpts...)

	var sampler sdktrace.Sampler
	if strings.ToLower(config.Sampler) == "ratio" {
		sampler = sdktrace.TraceIDRatioBased(config.SamplingRatio)
	} else {
		sampler = sdktrace.AlwaysSample()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	wrappedClient := &http.Client{
		Transport: otelhttp.NewTransport(&customTransport{
			underlying:    http.DefaultTransport,
			legacySemconv: config.LegacySemconv,
		}),
		Timeout:   30 * time.Second,
	}

	requestGoFn := func(call sobek.FunctionCall) sobek.Value {
		if len(call.Arguments) < 2 {
			return rt.ToValue(map[string]interface{}{"error": "method and url are required"})
		}
		method := strings.ToUpper(call.Arguments[0].String())
		url := call.Arguments[1].String()

		var bodyStr string
		var headers map[string]string
		customAttrs := make(map[string]interface{})

		var paramsObj *sobek.Object
		if len(call.Arguments) > 2 && (method == "GET" || method == "DELETE" || method == "HEAD" || method == "OPTIONS") {
			paramsObj, _ = call.Arguments[2].(*sobek.Object)
		} else if len(call.Arguments) > 3 {
			paramsObj, _ = call.Arguments[3].(*sobek.Object)
			if len(call.Arguments) > 2 {
				bodyStr = call.Arguments[2].String()
			}
		} else if len(call.Arguments) > 2 {
			bodyStr = call.Arguments[2].String()
		}

		if paramsObj != nil {
			if h := paramsObj.Get("headers"); h != nil {
				if hObj, ok := h.(*sobek.Object); ok {
					rt.ExportTo(hObj, &headers)
				}
			}
			if a := paramsObj.Get("attributes"); a != nil {
				if aObj, ok := a.(*sobek.Object); ok {
					rt.ExportTo(aObj, &customAttrs)
				}
			}
			if t := paramsObj.Get("tags"); t != nil {
				if tObj, ok := t.(*sobek.Object); ok {
					var tags map[string]interface{}
					rt.ExportTo(tObj, &tags)
					for k, v := range tags {
						if _, ok := customAttrs[k]; !ok {
							customAttrs[k] = v
						}
					}
				}
			}
		}

		var bodyReader io.Reader
		if bodyStr != "" {
			bodyReader = strings.NewReader(bodyStr)
		}
		
		reqCtx := context.WithValue(ctx, customAttributesKey, customAttrs)
		req, _ := http.NewRequestWithContext(reqCtx, method, url, bodyReader)
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := wrappedClient.Do(req)
		if err != nil {
			return rt.ToValue(map[string]interface{}{"error": err.Error(), "status_code": 500})
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		return rt.ToValue(map[string]interface{}{
			"status_code": resp.StatusCode,
			"status":      resp.StatusCode,
			"body":        string(respBody),
		})
	}

	makeMethodFn := func(m string) func(sobek.FunctionCall) sobek.Value {
		return func(call sobek.FunctionCall) sobek.Value {
			if len(call.Arguments) < 1 {
				return rt.ToValue(map[string]interface{}{"error": "url is required"})
			}
			newArgs := []sobek.Value{rt.ToValue(m), call.Arguments[0]}
			if len(call.Arguments) > 1 {
				newArgs = append(newArgs, call.Arguments[1:]...)
			}
			return requestGoFn(sobek.FunctionCall{Arguments: newArgs})
		}
	}

	httpObj.Set("request", rt.ToValue(requestGoFn))
	httpObj.Set("get", rt.ToValue(makeMethodFn("GET")))
	httpObj.Set("post", rt.ToValue(makeMethodFn("POST")))
	httpObj.Set("put", rt.ToValue(makeMethodFn("PUT")))
	httpObj.Set("del", rt.ToValue(makeMethodFn("DELETE")))
	httpObj.Set("patch", rt.ToValue(makeMethodFn("PATCH")))
}