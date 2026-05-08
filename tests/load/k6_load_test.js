// File: tests/load/k6_load_test.js
// Run with: k6 run tests/load/k6_load_test.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';

const cacheHits = new Counter('cache_hits');
const cacheMisses = new Counter('cache_misses');

// 70% cacheable prompts (repeated), 30% unique
const CACHEABLE_PROMPTS = [
  "What is the capital of France?",
  "Explain what a REST API is in simple terms.",
  "What are the SOLID principles in software engineering?",
  "How does garbage collection work in Go?",
  "What is the difference between SQL and NoSQL databases?",
  "Explain the CAP theorem.",
  "What is a JWT token and how does it work?",
  "Describe the producer-consumer pattern.",
  "What is Docker and why do developers use it?",
  "Explain microservices vs monolithic architecture.",
];

export const options = {
  stages: [
    { duration: '30s', target: 100 },   // ramp up
    { duration: '60s', target: 500 },   // hold at target
    { duration: '30s', target: 0 },     // ramp down
  ],
  thresholds: {
    'http_req_duration{p(99)}': ['<100'],   // p99 latency must be under 100ms
    'http_req_failed': ['<0.01'],            // <1% error rate
  },
};

const BASE_URL = __ENV.GATEWAY_URL || 'http://localhost:8080';
const API_KEY  = __ENV.GATEWAY_API_KEY || 'test-key-1';

export default function () {
  const useCacheable = Math.random() < 0.70;
  let prompt;

  if (useCacheable) {
    prompt = CACHEABLE_PROMPTS[Math.floor(Math.random() * CACHEABLE_PROMPTS.length)];
  } else {
    prompt = `Unique question ${Date.now()}_${Math.random()}: explain concept #${Math.floor(Math.random() * 10000)}`;
  }

  const payload = JSON.stringify({
    model: "gpt-4o",
    messages: [{ role: "user", content: prompt }],
    max_tokens: 150,
  });

  const params = {
    headers: {
      'Content-Type': 'application/json',
      'X-Gateway-API-Key': API_KEY,
      'X-Gateway-Team': 'load-test-team',
      'X-Gateway-Feature': 'k6-benchmark',
    },
    timeout: '5s',
  };

  const res = http.post(`${BASE_URL}/v1/chat/completions`, payload, params);

  check(res, {
    'status is 200': (r) => r.status === 200,
    'has gateway headers': (r) => r.headers['X-Gateway-Cache'] !== undefined,
  });

  if (res.headers['X-Gateway-Cache'] === 'hit') {
    cacheHits.add(1);
  } else {
    cacheMisses.add(1);
  }

  sleep(0.001);
}
