# xk6-otel-instrumentation

![GitHub Tag](https://img.shields.io/github/v/tag/AleksuKey/%20xk6-otel-instrumentation)

A highly flexible OpenTelemetry extension for Grafana k6, currently in active development (Beta phase).

Instead of using complex JavaScript wrapper functions or generating disconnected synthetic "noise" traces, this extension performs a native **monkey-patching intercept** on the core `k6/http` module directly within the Go runtime. 

Once initialized, any standard `http.get()`, `http.post()`, or `http.request()` call automatically spawns a real OpenTelemetry client span, injects official W3C `traceparent` headers across the wire, and exports precise nanosecond-level client-side traces to your APM backend (Jaeger, Tempo, OpenTelemetry Collector, etc.).

---

## 🚀 Key Features

* **Zero-Code Refactoring:** Seamlessly integrates into your existing test repositories. Your teams keep writing standard `k6/http` scripts while tracing happens transparently under the hood.
* **True E2E Network Hop Visibility:** Captures real client-side transport latency (DNS resolution, TLS handshake, and network transit) and displays your downstream application's internal spans nested perfectly inside the k6 root trace.
* **Dual-Write Semantic Conventions:** Supports both modern Stable and Legacy (retro-compatible) OpenTelemetry HTTP conventions simultaneously to ensure perfect alignment with older APM integrations (like older Python FastAPI setups).
* **High-Cardinality Span Naming Protection:** Native evaluation of `url.template` attributes to cleanly name client spans (e.g., `GET /api/v1/inventory/:id`) instead of raw high-cardinality URI paths.
* **Dynamic Request-Level Attributes:** Allows attaching on-the-fly custom metadata, tags, or IDs (such as Virtual User ID or iteration) directly to individual request spans using native k6 parameter options.
* **Dynamic Resource Attributes:** Pass any custom JSON object from JavaScript (e.g., `domain`, `service.environment`, `pipeline.id`), and the Go core will dynamically parse and bind them as OTel Resource Attributes.
* **Production-Ready Sampling Strategies:** Supports adaptive sampling rules (like `TraceIDRatioBased`) directly from the test script to optimize network bandwidth and APM storage during high-throughput load tests.
* **Agnostic OTLP Exporting:** Ships spans natively using the standard OTLP/gRPC protocol to any compliant OpenTelemetry ingestion engine.

---

## 🛠️ Installation & Compilation

To build a custom `k6` binary packed with this extension, you will need **Go** installed on your machine.

1. Install the official `xk6` bundler tool:
   ```bash
   go install go.k6.io/xk6/cmd/xk6@latest
   ```

2. Compile your customized `k6` binary fetching this extension directly from GitHub:
   ```bash
   xk6 build --with github.com/AleksuKey/xk6-otel-instrumentation@v0.1.0
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

When `.instrument()` is executed, the extension leverages the **Sobek JavaScript Runtime engine** inside k6 to target and swap out native JavaScript functions (`http.get`, `http.post`, etc.) with customized Go interceptors.

```text
 [ k6 Script ] ──> ( Native http.post ) ──> [ xk6 Interceptor (Go) ]
                                                      │
             ┌────────────────────────────────────────┴────────────────────────────────────────┐
             ▼                                                                                 ▼
[ OpenTelemetry Client Span ]                                                       [ HTTP Wire Request ]
- Injects standard W3C 'traceparent'                                                - Transmits HTTP Payload
- Tracks nanosecond metrics (DNS/TLS/Transit)                                       - Propagates Context to Python/Java/Go
- Captures custom request attributes & tags                                        - Triggers seamless downstream cascades
- Ships directly via OTLP/gRPC to Jaeger                                            
```

This hybrid execution guarantees that you maintain the incredible raw performance and low memory footprint of `k6`, while achieving 100% compliant distributed tracing context propagation.

---

## 📄 License

Distributed under the MIT License. See `LICENSE` for more information.

Copyright (c) 2026 AleksuKey.