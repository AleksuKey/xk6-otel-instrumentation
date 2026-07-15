import { sleep } from 'k6';
import http from 'k6/http';
import otel from 'k6/x/otel-instrumentation';

otel.instrument(http, {
  endpoint: __ENV.OTLP_ENDPOINT || 'localhost:4317',
  insecure: true,
  legacySemconv: true,
  resourceAttributes: {
    'service.name': 'k6-agent',
    'service.environment': __ENV.ENVIRONMENT || 'local',
    'test.type': 'constant-rate',
    'test.name': 'complex-post'
  }
});

export const options = {
  scenarios: {
    constant_request_rate: {
      executor: 'constant-arrival-rate',
      rate: 5,
      timeUnit: '1s',
      duration: '1m',
      preAllocatedVUs: 5,
      maxVUs: 10,
    },
  },
};

export default function () {
  const payload = JSON.stringify({
    discountId: 'SUMMER-2026',
    active: true,
    timestamp: Date.now()
  });

  http.post('http://localhost:8080/api/v1/discounts', payload, {
    headers: { 'Content-Type': 'application/json' },
    attributes: {
      'url.template': '/api/v1/discounts',
      'discount.id': 'SUMMER-2026',
      'is.loadtest': true
    }
  });
}