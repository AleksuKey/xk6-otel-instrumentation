import { sleep } from 'k6';
import http from 'k6/http';
import otel from 'k6/x/otel-instrumentation';

otel.instrument(http, {
  endpoint: __ENV.OTLP_ENDPOINT || 'localhost:4317',
  insecure: true,
  resourceAttributes: {
    'service.name': 'k6-agent',
    'service.environment': __ENV.ENVIRONMENT || 'local',
    'test.type': 'constant-rate',
    'test.name': 'basic-get'
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
  const customerIds = ['100200', '300400', '500600', '700800'];
  const randomCustomerId = customerIds[Math.floor(Math.random() * customerIds.length)];

  http.get(`http://localhost:8080/api/v1/inventory/${randomCustomerId}?countryCode=FR`, {
    headers: { 'Accept': 'application/json' },
    attributes: {
      'url.template': '/api/v1/inventory/:customerId',
      'customer.id': randomCustomerId,
      'vu.id': String(__VU),
      'vu.iteration': String(__ITER)
    }
  });
}