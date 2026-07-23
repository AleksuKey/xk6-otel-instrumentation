package xk6_otel_instrumentation

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/grafana/sobek"
	"go.k6.io/k6/v2/js/common"
	"go.k6.io/k6/v2/js/modules"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

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
	TimeoutSeconds     int                    `js:"timeoutSeconds"`
	MaxResponseBodyMB  int                    `js:"maxResponseBodyMB"`
}

var (
	providerOnce      sync.Once
	tracerProvider    *sdktrace.TracerProvider
	providerInitError error
)

func getOrCreateTracerProvider(config ClientConfig) (*sdktrace.TracerProvider, error) {
	providerOnce.Do(func() {
		tp, err := buildTracerProvider(config)
		if err != nil {
			providerInitError = err
			return
		}
		tracerProvider = tp
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
	return tracerProvider, providerInitError
}

func buildTracerProvider(config ClientConfig) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()
	attrs := attributesFromMap(config.ResourceAttributes)
	if config.ResourceAttributes["service.name"] == nil {
		attrs = append(attrs, attribute.String("service.name", "k6-agent"))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, err
	}
	exporterOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(config.Endpoint)}
	if config.Insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(buildSampler(config)),
		sdktrace.WithResource(res),
		sdktrace.WithSyncer(exporter),
	)
	return tp, nil
}

func buildSampler(config ClientConfig) sdktrace.Sampler {
	switch strings.ToLower(config.Sampler) {
	case "ratio":
		return sdktrace.TraceIDRatioBased(config.SamplingRatio)
	case "always_off":
		return sdktrace.NeverSample()
	default:
		return sdktrace.AlwaysSample()
	}
}

func attributesFromMap(m map[string]interface{}) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case string:
			attrs = append(attrs, attribute.String(k, val))
		case bool:
			attrs = append(attrs, attribute.Bool(k, val))
		case int64:
			attrs = append(attrs, attribute.Int64(k, val))
		case float64:
			attrs = append(attrs, attribute.Float64(k, val))
		default:
			attrs = append(attrs, attribute.String(k, fmt.Sprintf("%v", val)))
		}
	}
	return attrs
}

func isKnownMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE", "QUERY":
		return true
	}
	return false
}

func getHeaderValue(obj *sobek.Object, key string) string {
	if obj == nil {
		return ""
	}
	targetLower := strings.ToLower(key)
	for _, k := range obj.Keys() {
		if strings.ToLower(k) == targetLower {
			val := obj.Get(k)
			if val != nil && !sobek.IsUndefined(val) && !sobek.IsNull(val) {
				return val.String()
			}
		}
	}
	return ""
}

func (m *OtelModule) Instrument(httpObj *sobek.Object, config ClientConfig) {
	rt := m.vu.Runtime()
	tp, err := getOrCreateTracerProvider(config)
	if err != nil {
		common.Throw(rt, err)
		return
	}

	origReqVal := httpObj.Get("request")
	origReqFn, ok := sobek.AssertFunction(origReqVal)
	if !ok {
		common.Throw(rt, errors.New("http.request is not a function"))
		return
	}

	tracer := tp.Tracer("xk6-otel-instrumentation")
	requestGoFn := m.makeRequestFn(rt, tracer, origReqFn, config.LegacySemconv)

	makeMethodFn := func(method string) func(sobek.FunctionCall) sobek.Value {
		return func(call sobek.FunctionCall) sobek.Value {
			if len(call.Arguments) < 1 {
				return rt.ToValue(map[string]interface{}{"error": "url is required"})
			}
			newArgs := append([]sobek.Value{rt.ToValue(method), call.Arguments[0]}, call.Arguments[1:]...)
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

func (m *OtelModule) makeRequestFn(
	rt *sobek.Runtime,
	tracer trace.Tracer,
	origReqFn sobek.Callable,
	legacySemconv bool,
) func(sobek.FunctionCall) sobek.Value {
	return func(call sobek.FunctionCall) sobek.Value {
		if len(call.Arguments) < 2 {
			return rt.ToValue(map[string]interface{}{"error": "method and url are required"})
		}

		method := strings.ToUpper(call.Arguments[0].String())
		rawURL := call.Arguments[1].String()

		var bodyVal sobek.Value = sobek.Undefined()
		var paramsObj *sobek.Object

		switch method {
		case "GET", "HEAD", "DELETE", "OPTIONS":
			if len(call.Arguments) > 2 {
				if obj, ok := call.Arguments[2].(*sobek.Object); ok && obj != nil && !sobek.IsUndefined(call.Arguments[2]) && !sobek.IsNull(call.Arguments[2]) {
					paramsObj = obj
				} else {
					bodyVal = call.Arguments[2]
				}
			}
			if len(call.Arguments) > 3 {
				if obj, ok := call.Arguments[3].(*sobek.Object); ok && obj != nil {
					paramsObj = obj
				}
			}
		default:
			if len(call.Arguments) > 2 {
				bodyVal = call.Arguments[2]
			}
			if len(call.Arguments) > 3 {
				if obj, ok := call.Arguments[3].(*sobek.Object); ok && obj != nil && !sobek.IsUndefined(call.Arguments[3]) && !sobek.IsNull(call.Arguments[3]) {
					paramsObj = obj
				}
			}
		}

		if paramsObj == nil {
			paramsObj = rt.NewObject()
		}

		customAttrs := make(map[string]interface{})
		if a := paramsObj.Get("attributes"); a != nil && !sobek.IsUndefined(a) && !sobek.IsNull(a) {
			if aObj, ok := a.(*sobek.Object); ok && aObj != nil {
				for _, k := range aObj.Keys() {
					val := aObj.Get(k)
					if val != nil {
						customAttrs[k] = val.Export()
					}
				}
			}
		}
		if t := paramsObj.Get("tags"); t != nil && !sobek.IsUndefined(t) && !sobek.IsNull(t) {
			if tObj, ok := t.(*sobek.Object); ok && tObj != nil {
				for _, k := range tObj.Keys() {
					if _, exists := customAttrs[k]; !exists {
						val := tObj.Get(k)
						if val != nil {
							customAttrs[k] = val.Export()
						}
					}
				}
			}
		}

		var headersObj *sobek.Object
		if h := paramsObj.Get("headers"); h != nil && !sobek.IsUndefined(h) && !sobek.IsNull(h) {
			if hObj, ok := h.(*sobek.Object); ok && hObj != nil {
				headersObj = hObj
			}
		}
		if headersObj == nil {
			headersObj = rt.NewObject()
			_ = paramsObj.Set("headers", headersObj)
		}

		urlTemplate, _ := customAttrs["url.template"].(string)
		spanName := method
		if urlTemplate != "" {
			spanName = fmt.Sprintf("%s %s", method, urlTemplate)
		}

		ctx, span := tracer.Start(context.Background(), spanName, trace.WithSpanKind(trace.SpanKindClient))
		defer span.End()

		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(ctx, carrier)
		for k, v := range carrier {
			_ = headersObj.Set(k, v)
		}

		setSpanAttributes(span, method, rawURL, urlTemplate, headersObj, customAttrs, legacySemconv)

		args := []sobek.Value{
			rt.ToValue(method),
			rt.ToValue(rawURL),
			bodyVal,
			paramsObj,
		}

		resVal, err := origReqFn(sobek.Undefined(), args...)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String("error.type", "request_error"))
			span.RecordError(err)
			return rt.ToValue(map[string]interface{}{"error": err.Error(), "status_code": 0})
		}

		if resObj, ok := resVal.(*sobek.Object); ok && resObj != nil {
			// Extract status code
			statusVal := resObj.Get("status")
			if statusVal != nil && !sobek.IsUndefined(statusVal) && !sobek.IsNull(statusVal) {
				statusCode := statusVal.ToInteger()
				span.SetAttributes(attribute.Int64("http.response.status_code", statusCode))
				if legacySemconv {
					span.SetAttributes(attribute.Int64("http.status_code", statusCode))
				}
				if statusCode >= 400 {
					span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
					span.SetAttributes(attribute.String("error.type", strconv.FormatInt(statusCode, 10)))
				}
			}

			// Extract network protocol version (e.g. "HTTP/1.1" -> "1.1")
			protoVal := resObj.Get("proto")
			if protoVal != nil && !sobek.IsUndefined(protoVal) && !sobek.IsNull(protoVal) {
				protoStr := protoVal.String()
				protoVer := strings.TrimPrefix(protoStr, "HTTP/")
				if protoVer != "" {
					span.SetAttributes(
						attribute.String("network.protocol.name", "http"),
						attribute.String("network.protocol.version", protoVer),
					)
					if legacySemconv {
						span.SetAttributes(attribute.String("net.protocol.version", protoVer))
					}
				}
			}
		}

		return resVal
	}
}

func setSpanAttributes(
	span trace.Span,
	method, rawURL, urlTemplate string,
	headersObj *sobek.Object,
	customAttrs map[string]interface{},
	legacySemconv bool,
) {
	if !span.IsRecording() {
		return
	}

	parsedURL, err := url.Parse(rawURL)
	scheme := "http"
	host := ""
	path := "/"
	portStr := ""
	hostname := rawURL

	if err == nil {
		if parsedURL.Scheme != "" {
			scheme = parsedURL.Scheme
		}
		if parsedURL.Host != "" {
			host = parsedURL.Host
		}
		path = parsedURL.Path
		if path == "" {
			path = "/"
		}
		portStr = parsedURL.Port()
		hostname = parsedURL.Hostname()
	}

	methodAttr := method
	methodOriginalAttr := ""
	if !isKnownMethod(method) {
		methodAttr = "_OTHER"
		methodOriginalAttr = method
		method = "HTTP"
	}

	userAgent := getHeaderValue(headersObj, "User-Agent")
	if userAgent == "" {
		userAgent = "k6-agent"
	}

	span.SetAttributes(
		attribute.String("http.request.method", methodAttr),
		attribute.String("url.full", rawURL),
		attribute.String("url.path", path),
		attribute.String("url.scheme", scheme),
		attribute.String("server.address", hostname),
		attribute.String("user_agent.original", userAgent),
	)

	if methodOriginalAttr != "" {
		span.SetAttributes(attribute.String("http.request.method_original", methodOriginalAttr))
	}
	if urlTemplate != "" {
		span.SetAttributes(attribute.String("url.template", urlTemplate))
	}

	if legacySemconv {
		span.SetAttributes(
			attribute.String("http.method", methodAttr),
			attribute.String("http.url", rawURL),
			attribute.String("http.scheme", scheme),
			attribute.String("http.host", host),
			attribute.String("http.user_agent", userAgent),
			attribute.String("net.peer.name", hostname),
		)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		port = 80
		if scheme == "https" {
			port = 443
		}
	}
	span.SetAttributes(attribute.Int64("server.port", int64(port)))
	if legacySemconv {
		span.SetAttributes(attribute.Int64("net.peer.port", int64(port)))
	}

	// Capture opt-in request headers using official standard: http.request.header.<key> -> []string
	for _, h := range []string{"Accept", "Connection", "X-SampleRatio"} {
		if val := getHeaderValue(headersObj, h); val != "" {
			headerKey := fmt.Sprintf("http.request.header.%s", strings.ToLower(h))
			span.SetAttributes(attribute.StringSlice(headerKey, []string{val}))
		}
	}

	filteredCustomAttrs := make(map[string]interface{}, len(customAttrs))
	for k, v := range customAttrs {
		if k != "url.template" {
			filteredCustomAttrs[k] = v
		}
	}
	span.SetAttributes(attributesFromMap(filteredCustomAttrs)...)
}