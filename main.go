package xk6_otel_instrumentation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sobek"
	"go.k6.io/k6/v2/js/common"
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

// ---------------------------------------------------------------------
// Módulo k6 (boilerplate estándar de cualquier extensión)
// ---------------------------------------------------------------------

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

// ClientConfig es la configuración común de OpenTelemetry. No tiene
// nada específico de HTTP a propósito: cuando en el futuro se
// instrumente Kafka, Redis, SQL, etc., se reutilizará este mismo
// struct (o uno muy parecido) para el endpoint, el sampler y los
// resourceAttributes.
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

// ===========================================================================
// NÚCLEO DE OPENTELEMETRY (protocolo-agnóstico)
//
// Todo lo de este bloque no sabe nada de HTTP. Monta el TracerProvider,
// el exportador OTLP y el sampler. Cualquier instrumentación futura
// (Kafka, RabbitMQ, Redis, SQL...) llamaría a getOrCreateTracerProvider
// para obtener el mismo provider, sin tener que volver a escribir esta
// lógica.
// ===========================================================================

// k6 crea una instancia de OtelModule por cada VU, pero el
// TracerProvider de OpenTelemetry es estado GLOBAL del proceso
// (otel.SetTracerProvider). Si cada VU creara el suyo, tendríamos
// conexiones gRPC duplicadas y una condición de carrera al escribir
// el estado global. Por eso se crea una única vez con sync.Once y se
// comparte entre todos los VUs.
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
		return nil, fmt.Errorf("no se pudo construir el resource de OpenTelemetry: %w", err)
	}

	exporterOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(config.Endpoint)}
	if config.Insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("no se pudo crear el exportador OTLP hacia %q: %w", config.Endpoint, err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(buildSampler(config)),
		sdktrace.WithResource(res),
		// No hay ninguna llamada a shutdown() desde el script k6, así
		// que en vez de esperar a un flush final (que se puede perder
		// si k6 mata el proceso de golpe), forzamos un flush frecuente:
		// cada 2s o cada 50 spans, lo que ocurra antes. Así, aunque el
		// test termine abruptamente, como mucho se pierden los últimos
		// 2 segundos de trazas en vez de todo el buffer.
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(50),
		),
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

// attributesFromMap convierte un map genérico (tal y como llega desde
// JS) a atributos de OpenTelemetry. La usa tanto el resource como los
// atributos custom de cada span, y la reutilizará cualquier
// instrumentación futura (Kafka, SQL, etc.).
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

// classifyError da una categoría legible a errores de red usando
// errors.As/errors.Is en vez de comparar texto de mensajes (frágil).
// Sirve igual para HTTP que para cualquier protocolo sobre TCP
// (Kafka, Redis, SQL...), así que se queda en el bloque genérico.
func classifyError(err error) string {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "unknown_host"
	}

	if strings.Contains(err.Error(), "connection refused") {
		return "connection_refused"
	}

	return "request_error"
}

// ===========================================================================
// INSTRUMENTACIÓN HTTP
//
// A partir de aquí, todo es específico del módulo k6/http. El día que
// se añada Kafka/Redis/SQL, iría en su propio bloque (o incluso su
// propio archivo) con su propio Instrument*, reutilizando siempre las
// funciones de arriba.
// ===========================================================================

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
		c.setRequestAttributes(span, req)
	}

	resp, err := c.underlying.RoundTrip(req)
	if err != nil {
		if span.IsRecording() {
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String("error.type", classifyError(err)))
			span.RecordError(err)
		}
		return nil, err
	}

	if resp != nil && span.IsRecording() {
		c.setResponseAttributes(span, resp)
	}

	return resp, nil
}

func (c *customTransport) setRequestAttributes(span trace.Span, req *http.Request) {
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

	customAttrs, _ := req.Context().Value(customAttributesKey).(map[string]interface{})

	urlTemplate := ""
	if ut, ok := customAttrs["url.template"].(string); ok {
		urlTemplate = ut
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

	c.setPortAttributes(span, req.URL.Port(), scheme)

	// Atributos custom del script k6 (sin "url.template", que ya se
	// ha usado arriba para el nombre del span).
	filteredCustomAttrs := make(map[string]interface{}, len(customAttrs))
	for k, v := range customAttrs {
		if k != "url.template" {
			filteredCustomAttrs[k] = v
		}
	}
	span.SetAttributes(attributesFromMap(filteredCustomAttrs)...)

	for _, h := range []string{"Accept", "Connection", "X-SampleRatio"} {
		if val := req.Header.Get(h); val != "" {
			span.SetAttributes(attribute.String(h, val))
		}
	}
}

func (c *customTransport) setPortAttributes(span trace.Span, portStr, scheme string) {
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		port = 80
		if scheme == "https" {
			port = 443
		}
	}

	span.SetAttributes(attribute.Int64("server.port", int64(port)))
	if c.legacySemconv {
		span.SetAttributes(attribute.Int64("net.peer.port", int64(port)))
	}
}

func (c *customTransport) setResponseAttributes(span trace.Span, resp *http.Response) {
	statusCode := int64(resp.StatusCode)

	span.SetAttributes(attribute.Int64("http.response.status_code", statusCode))
	if c.legacySemconv {
		span.SetAttributes(attribute.Int64("http.status_code", statusCode))
	}

	protoVer := strings.TrimPrefix(resp.Proto, "HTTP/")
	span.SetAttributes(attribute.String("network.protocol.version", protoVer))
	if c.legacySemconv {
		span.SetAttributes(attribute.String("net.protocol.version", protoVer))
	}

	if statusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", statusCode))
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%d", statusCode)))
	}
}

// Instrument mantiene exactamente la misma firma y forma de uso que
// antes: otel.instrument(http, { ... }). Si algo falla al montar el
// TracerProvider (endpoint mal formado, exportador que no arranca,
// etc.), se lanza como excepción de JS con common.Throw en vez de
// ignorarse en silencio, así te enteras al vuelo en el propio script.
func (m *OtelModule) Instrument(httpObj *sobek.Object, config ClientConfig) {
	rt := m.vu.Runtime()

	if _, err := getOrCreateTracerProvider(config); err != nil {
		common.Throw(rt, err)
		return
	}

	timeout := 30 * time.Second
	if config.TimeoutSeconds > 0 {
		timeout = time.Duration(config.TimeoutSeconds) * time.Second
	}

	maxBodyBytes := int64(10 * 1024 * 1024) // 10 MB por defecto
	if config.MaxResponseBodyMB > 0 {
		maxBodyBytes = int64(config.MaxResponseBodyMB) * 1024 * 1024
	}

	wrappedClient := &http.Client{
		Transport: otelhttp.NewTransport(&customTransport{
			underlying:    http.DefaultTransport,
			legacySemconv: config.LegacySemconv,
		}),
		Timeout: timeout,
	}

	requestGoFn := m.makeRequestFn(rt, wrappedClient, maxBodyBytes)

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

// makeRequestFn construye la función Go que se expone como
// http.request(method, url, [body], [params]) en el script k6.
//
// Para saber si un argumento extra es el body o el objeto de params
// miramos su tipo real (¿es un *sobek.Object?) en vez de adivinarlo
// por posición + método HTTP, que era lo que hacía fallar antes con
// llamadas tipo post(url, paramsObj) sin body.
func (m *OtelModule) makeRequestFn(rt *sobek.Runtime, client *http.Client, maxBodyBytes int64) func(sobek.FunctionCall) sobek.Value {
	return func(call sobek.FunctionCall) sobek.Value {
		if len(call.Arguments) < 2 {
			return rt.ToValue(map[string]interface{}{"error": "method and url are required"})
		}

		method := strings.ToUpper(call.Arguments[0].String())
		url := call.Arguments[1].String()

		var bodyStr string
		var paramsObj *sobek.Object

		for _, arg := range call.Arguments[2:] {
			if obj, ok := arg.(*sobek.Object); ok && obj != nil {
				paramsObj = obj
				continue
			}
			bodyStr = arg.String()
		}

		var headers map[string]string
		customAttrs := make(map[string]interface{})

		if paramsObj != nil {
			if h := paramsObj.Get("headers"); h != nil {
				if hObj, ok := h.(*sobek.Object); ok {
					_ = rt.ExportTo(hObj, &headers)
				}
			}
			if a := paramsObj.Get("attributes"); a != nil {
				if aObj, ok := a.(*sobek.Object); ok {
					_ = rt.ExportTo(aObj, &customAttrs)
				}
			}
			if t := paramsObj.Get("tags"); t != nil {
				if tObj, ok := t.(*sobek.Object); ok {
					var tags map[string]interface{}
					_ = rt.ExportTo(tObj, &tags)
					for k, v := range tags {
						if _, exists := customAttrs[k]; !exists {
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

		reqCtx := context.WithValue(context.Background(), customAttributesKey, customAttrs)
		req, err := http.NewRequestWithContext(reqCtx, method, url, bodyReader)
		if err != nil {
			return rt.ToValue(map[string]interface{}{"error": err.Error(), "status_code": 0})
		}

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			return rt.ToValue(map[string]interface{}{"error": err.Error(), "status_code": 0})
		}
		defer resp.Body.Close()

		limitedBody := io.LimitReader(resp.Body, maxBodyBytes)
		respBody, err := io.ReadAll(limitedBody)
		if err != nil {
			return rt.ToValue(map[string]interface{}{"error": err.Error(), "status_code": resp.StatusCode})
		}

		return rt.ToValue(map[string]interface{}{
			"status_code": resp.StatusCode,
			"status":      resp.StatusCode,
			"body":        string(respBody),
		})
	}
}
