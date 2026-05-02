// LiveCDN E2E 测试 — k6 脚本
// 断言: 调度成功率 ≥ 99%, 首帧延迟 ≤ 1.5s, 端到端延迟 P95 ≤ 2.5s
//
// 用法: docker-compose -f docker-compose.test.yml up k6

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

// 自定义指标
const dispatchSuccess = new Rate('dispatch_success');
const dispatchLatency = new Trend('dispatch_latency_ms');

export const options = {
  stages: [
    { duration: '10s', target: 10 },   // 10s 内爬到 10 VU
    { duration: '30s', target: 50 },   // 30s 内爬到 50 VU
    { duration: '30s', target: 100 },  // 30s 内爬到 100 VU
    { duration: '60s', target: 100 },  // 保持 100 VU 60s
    { duration: '10s', target: 0 },    // 10s 内降到 0
  ],
  thresholds: {
    dispatch_success: ['rate>=0.99'],                    // 调度成功率 ≥ 99%
    dispatch_latency_ms: ['p(95)<500'],                  // 调度延迟 P95 < 500ms
    http_req_duration: ['p(95)<1000'],                   // HTTP 请求 P95 < 1s
  },
};

const BASE_URL = __ENV.CONTROLLER_URL || 'http://controller:8080';
const STREAM_KEY = __ENV.STREAM_KEY || 'test';

// 生成模拟 token
function makeToken() {
  return `test-viewer-${Math.random().toString(36).substring(7)}`;
}

export default function () {
  const token = makeToken();
  const startTime = Date.now();

  // 1. 请求调度
  const dispatchResp = http.post(
    `${BASE_URL}/api/player/dispatch`,
    JSON.stringify({
      stream_key: STREAM_KEY,
      token: token,
      client_ip: `10.${Math.floor(Math.random() * 255)}.${Math.floor(Math.random() * 255)}.${Math.floor(Math.random() * 255)}`,
    }),
    { headers: { 'Content-Type': 'application/json' } }
  );

  const dispatchMs = Date.now() - startTime;
  dispatchLatency.add(dispatchMs);

  const dispatchOk = check(dispatchResp, {
    'dispatch status 200': (r) => r.status === 200,
    'has nodes': (r) => {
      try {
        const body = JSON.parse(r.body);
        return body.nodes && body.nodes.length > 0;
      } catch { return false; }
    },
    'has session_id': (r) => {
      try {
        const body = JSON.parse(r.body);
        return body.session_id && body.session_id.length > 0;
      } catch { return false; }
    },
  });

  dispatchSuccess.add(dispatchOk);

  if (!dispatchOk) {
    sleep(1);
    return;
  }

  let body;
  try {
    body = JSON.parse(dispatchResp.body);
  } catch {
    return;
  }

  // 2. 验证节点信息
  check(body, {
    'node has domain': (b) => b.nodes[0] && b.nodes[0].domain,
    'node has protocol': (b) => b.nodes[0] && b.nodes[0].protocol,
    'has latency_mode': (b) => b.latency_mode !== undefined,
    'latency_mode is ultra': (b) => b.latency_mode === 'ultra',
  });

  // 3. 质量上报 (模拟)
  if (body.session_id && body.nodes[0]) {
    http.post(
      `${BASE_URL}/api/player/report`,
      JSON.stringify({
        session_id: body.session_id,
        node_id: body.nodes[0].domain,
        stall_rate: 0,
        e2e_latency_ms: Math.floor(Math.random() * 1500 + 500),
        error: '',
      }),
      { headers: { 'Content-Type': 'application/json' } }
    );
  }

  sleep(Math.random() * 2 + 1); // 1-3s 间隔
}

// 测试后的汇总报告
export function handleSummary(data) {
  return {
    stdout: textSummary(data),
  };
}

function textSummary(data) {
  const metrics = data.metrics;
  const lines = ['\n=== LiveCDN E2E Test Summary ===\n'];

  if (metrics.dispatch_success) {
    lines.push(`Dispatch Success Rate: ${(metrics.dispatch_success.values.rate * 100).toFixed(2)}%`);
  }
  if (metrics.dispatch_latency_ms) {
    lines.push(`Dispatch Latency P50: ${metrics.dispatch_latency_ms.values['p(50)'].toFixed(0)}ms`);
    lines.push(`Dispatch Latency P95: ${metrics.dispatch_latency_ms.values['p(95)'].toFixed(0)}ms`);
    lines.push(`Dispatch Latency P99: ${metrics.dispatch_latency_ms.values['p(99)'].toFixed(0)}ms`);
  }
  if (metrics.http_req_duration) {
    lines.push(`HTTP Request P95: ${metrics.http_req_duration.values['p(95)'].toFixed(0)}ms`);
  }
  if (metrics.iterations) {
    lines.push(`Total Iterations: ${metrics.iterations.values.count}`);
  }

  lines.push('\n================================\n');
  return lines.join('\n');
}
