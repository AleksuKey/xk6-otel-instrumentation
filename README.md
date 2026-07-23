# xk6-otel-instrumentation

![GitHub Tag](https://img.shields.io/github/v/tag/AleksuKey/xk6-otel-instrumentation)

A highly flexible OpenTelemetry extension for Grafana k6, currently in active development (Stable).

Instead of using complex JavaScript wrapper functions or generating disconnected synthetic "noise" traces, this extension performs a native **monkey-patching intercept** on the core `k6/http` module directly within the Go runtime. 

Once initialized, any standard `http.get()`, `http.post()`, or `http.request()` call automatically spawns a real OpenTelemetry client span, injects official W3C `traceparent` headers across the wire, and exports precise nanosecond-level client-side traces to your APM backend (Jaeger, Tempo, OpenTelemetry Collector, etc.).

---

## 🚀 Key Features

* **Zero-Code Native Intercept:** Drops seamlessly into existing test suites—tracing happens transparently under the hood without rewriting standard `k6/http` calls or logic.
* **Zero Metric Loss:** Delegates execution back to k6's core engine, preserving 100% of native k6 metrics (`http_req_duration`, CLI summaries, and standard metric exporters).
* **OTel Spec & Low-Cardinality Naming:** Aligns strictly with OpenTelemetry HTTP Client conventions (supporting both Stable & Legacy dual-write) and uses `url.template` to prevent high-cardinality span name explosion.
* **Full Context Propagation & Metadata:** Injects official W3C `traceparent` headers across the wire while mapping custom request tags, VU metadata, and global resource attributes directly to spans.
* **Native OTLP/gRPC Exporting:** Ships spans via gRPC to any OTel-compliant backend (Jaeger, Tempo, OTel Collector) with built-in ratio sampling support.

---

## 🛠️ Installation & Compilation

To build a custom `k6` binary packed with this extension, you will need **Go** installed on your machine.

1. Install the official `xk6` bundler tool:
   ```bash
   go install go.k6.io/xk6/cmd/xk6@latest
   ```

2. Compile your customized `k6` binary fetching this extension directly from GitHub:
   ```bash
   xk6 build --with https://github.com/AleksuKey/xk6-otel-instrumentation@v1.2.0
   ```

This will output a native `./k6` executable binary in your current working directory.

---

## 💻 Usage Quickstart

Simply import the extension and call the `.instrument()` method at the very beginning of your k6 script (the *init context*). Pass your native `http` module along with your custom configuration block.

```javascript
import { sleep } from 'k6';
import http from 'k6/http';
import otel from 'k6/x/otel-instrumentation';

otel.instrument(http, {
  endpoint: __ENV.OTLP_ENDPOINT || "localhost:4317",
  insecure: true,
  sampler: "ratio", 
  samplingRatio: 0.2, 
  legacySemconv: true, 
  resourceAttributes: {
    "service.product": "k6",
    "service.name": "k6-agent",
    "service.environment": __ENV.ENVIRONMENT || "trn",
    "custom.pipeline.id": __ENV.BUILD_BUILDID || "local-run"
  }
});

export const options = {
  vus: 1,
  duration: '10s',
};

export default function (data) {
  http.get('http://localhost:8000/api/v1/inventory/12345', {
    headers: { 'Accept': 'application/json' },
    attributes: {
      'url.template': '/api/v1/inventory/:id',
      'client.id': '111222333',
      'test.iteration': String(__ITER),
      'is.loadtest': true
    }
  });

  sleep(1);
}
```

---

## ⚙️ Configuration Options

The `.instrument(httpModule, config)` method accepts a flexible configuration object with the following parameters:

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `endpoint` | `String` | *(Required)* | The OTLP gRPC endpoint where traces will be sent (e.g., `localhost:4317`). Do not include protocols like `http://`. |
| `insecure` | `Boolean` | `false` | Disables client transport security (TLS) for the gRPC exporter connection. Set to `true` for local development. |
| `sampler` | `String` | `"always_on"` | Trace sampling strategy. Options: `"always_on"`, `"always_off"`, or `"ratio"`. |
| `samplingRatio` | `Float` | `1.0` | Controlled sample rate ratio. Used only if `sampler` is set to `"ratio"`. Accepts values between `0.0` (0%) and `1.0` (100%). |
| `legacySemconv` | `Boolean` | `false` | Enables Dual-Write mode, writing both modern Stable (e.g., `http.request.method`) and Legacy (e.g., `http.method`) semantic conventions side-by-side. Useful for older APM environments. |
| `timeoutSeconds` | `Integer` | `30` | Network timeout threshold in seconds for outbound HTTP requests. |
| `maxResponseBodyMB` | `Integer` | `10` | Maximum allocation safety ceiling in Megabytes allowed for responses to protect k6 memory from high-throughput leaks. |
| `resourceAttributes` | `Object` | `{}` | A key-value map of custom strings, booleans, integers, or floats to register as OpenTelemetry global process attributes. |

---

## 🏷️ Custom Request-Level Span Attributes

You can dynamically inject custom metadata on a per-request basis. This is incredibly helpful to map virtual users, database targets, or business IDs directly onto your trace timeline.

To attach attributes to a specific request span, use the `attributes` or `tags` properties inside the k6 `params` argument:

```javascript
// Using custom OpenTelemetry attributes
http.get('http://localhost:8000/api/v1/inventory', {
  attributes: {
    "url.template": "/api/v1/inventory",
    "my.custom.attribute": "custom-value",
    "user.tier": "premium"
  }
});

// Using standard k6 tags (they will be automatically converted to OTel attributes)
http.post('http://localhost:8000/api/v1/discounts', JSON.stringify({ discountId: "DISC-9" }), {
  headers: { "Content-Type": "application/json" },
  tags: {
    "url.template": "/api/v1/discounts",
    "action.name": "activate-discount",
    "custom.tag": "discounts-engine"
  }
});
```

---

## 📂 Runnable Examples

We provide a collection of ready-to-use script templates to help you test the extension in your environment instantly. You can find them in the `examples/` directory:

* [Basic GET Request Template](./examples/basic-get.js): Shows how to trace simple GET requests, pass parameters, and preserve high-cardinality protection using `url.template`.
* [Complex POST Request Template](./examples/complex-post.js): Demonstrates tracing structured POST JSON payloads and converting standard k6 `tags` blocks into clean OTel span attributes.

---

## 🧠 How it Works Under the Hood

When `.instrument()` is executed, the extension captures k6's native JavaScript `http.request` function. When an HTTP call is executed from your test script:

1. The interceptor creates an OpenTelemetry client span and sets initial request attributes.
2. It injects standard W3C `traceparent` and `tracestate` headers into the request headers.
3. It **delegates execution back to k6's native HTTP engine**, ensuring all current and future native k6 HTTP metrics are recorded accurately.
4. Once the response is returned, it extracts response attributes, updates span status/errors, and closes the span.

```text
  [ k6 Script (http.get / http.post) ]
                   │
                   ▼
  ┌────────────────────────────────────────┐
  │ xk6 Interceptor                        │
  │ 1. Starts OTel Client Span             │
  │ 2. Injects W3C 'traceparent' Header    │
  └──────────────────┬─────────────────────┘
                     │ Delegates Execution
                     ▼
  ┌────────────────────────────────────────┐
  │ Native k6 Core HTTP Engine             │
  │ 1. Transmits HTTP Request over Wire    │
  │ 2. Emits ALL Native k6 HTTP Metrics    │
  │    (http_req_duration, http_reqs, etc) │
  └──────────────────┬─────────────────────┘
                     │ Returns Response Object
                     ▼
  ┌────────────────────────────────────────┐
  │ xk6 Interceptor                        │
  │ 1. Records Status, Errors, & Proto     │
  │ 2. Closes Span & Exports via OTLP/gRPC │
  └────────────────────────────────────────┘
```

This hybrid execution guarantees that you maintain the raw performance and complete native metric reporting of k6 while achieving 100% compliant distributed tracing context propagation.

---

## 📄 License

Distributed under the MIT License. See `LICENSE` for more information.

Copyright (c) 2026 AleksuKey.