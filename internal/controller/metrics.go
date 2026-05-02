package controller

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/user/live-cdn/internal/common"
)

// Metrics 收集和导出 Prometheus 格式的指标
type Metrics struct {
	// Counters
	DispatchTotal    atomic.Int64
	DispatchFail     atomic.Int64
	RegisterTotal    atomic.Int64
	HeartbeatTotal   atomic.Int64
	QualityReported  atomic.Int64
	StreamStartTotal atomic.Int64
	StreamStopTotal  atomic.Int64
	KeyRequests      atomic.Int64

	// Gauges (需要定期从 store 更新)
	OnlineNodes    atomic.Int64
	TotalNodes     atomic.Int64
	LiveStreams    atomic.Int64
	TotalUsers     atomic.Int64
	TotalBWMbps    atomic.Int64

	// Histogram-like (简化版，用桶计数)
	DispatchLatencyBuckets [5]atomic.Int64 // <5ms, <10ms, <25ms, <50ms, >50ms
}

func NewMetrics() *Metrics {
	return &Metrics{}
}

// RecordDispatchLatency 记录调度延迟
func (m *Metrics) RecordDispatchLatency(d time.Duration) {
	ms := d.Milliseconds()
	switch {
	case ms < 5:
		m.DispatchLatencyBuckets[0].Add(1)
	case ms < 10:
		m.DispatchLatencyBuckets[1].Add(1)
	case ms < 25:
		m.DispatchLatencyBuckets[2].Add(1)
	case ms < 50:
		m.DispatchLatencyBuckets[3].Add(1)
	default:
		m.DispatchLatencyBuckets[4].Add(1)
	}
}

// UpdateFromStore 从 store 更新 gauge 指标
func (m *Metrics) UpdateFromStore(store *MemoryStore) {
	nodes := store.GetAllNodes()
	streams := store.GetLiveStreams()

	online := int64(0)
	totalUsers := int64(0)
	totalBW := int64(0)
	for _, n := range nodes {
		if IsNodeHealthy(n) {
			online++
		}
		totalUsers += int64(n.OnlineUsers)
		totalBW += n.BWUsed
	}

	m.OnlineNodes.Store(online)
	m.TotalNodes.Store(int64(len(nodes)))
	m.LiveStreams.Store(int64(len(streams)))
	m.TotalUsers.Store(totalUsers)
	m.TotalBWMbps.Store(totalBW * 8 / 1_000_000)
}

// ServeHTTP 处理 /metrics 请求 (Prometheus text format)
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	fmt.Fprintf(w, "# HELP livecdn_dispatch_total Total dispatch requests\n")
	fmt.Fprintf(w, "# TYPE livecdn_dispatch_total counter\n")
	fmt.Fprintf(w, "livecdn_dispatch_total %d\n\n", m.DispatchTotal.Load())

	fmt.Fprintf(w, "# HELP livecdn_dispatch_fail_total Failed dispatch requests\n")
	fmt.Fprintf(w, "# TYPE livecdn_dispatch_fail_total counter\n")
	fmt.Fprintf(w, "livecdn_dispatch_fail_total %d\n\n", m.DispatchFail.Load())

	fmt.Fprintf(w, "# HELP livecdn_dispatch_latency Bucket counts\n")
	fmt.Fprintf(w, "# TYPE livecdn_dispatch_latency histogram\n")
	le := []string{"5", "10", "25", "50", "+Inf"}
	cumulative := int64(0)
	for i, bucket := range le {
		cumulative += m.DispatchLatencyBuckets[i].Load()
		fmt.Fprintf(w, "livecdn_dispatch_latency_bucket{le=\"%s\"} %d\n", bucket, cumulative)
	}
	fmt.Fprintf(w, "livecdn_dispatch_latency_count %d\n", m.DispatchTotal.Load())
	fmt.Fprintf(w, "livecdn_dispatch_latency_sum 0\n\n")

	fmt.Fprintf(w, "# HELP livecdn_nodes_online Currently online nodes\n")
	fmt.Fprintf(w, "# TYPE livecdn_nodes_online gauge\n")
	fmt.Fprintf(w, "livecdn_nodes_online %d\n\n", m.OnlineNodes.Load())

	fmt.Fprintf(w, "# HELP livecdn_nodes_total Total registered nodes\n")
	fmt.Fprintf(w, "# TYPE livecdn_nodes_total gauge\n")
	fmt.Fprintf(w, "livecdn_nodes_total %d\n\n", m.TotalNodes.Load())

	fmt.Fprintf(w, "# HELP livecdn_streams_live Currently live streams\n")
	fmt.Fprintf(w, "# TYPE livecdn_streams_live gauge\n")
	fmt.Fprintf(w, "livecdn_streams_live %d\n\n", m.LiveStreams.Load())

	fmt.Fprintf(w, "# HELP livecdn_users_total Current viewers\n")
	fmt.Fprintf(w, "# TYPE livecdn_users_total gauge\n")
	fmt.Fprintf(w, "livecdn_users_total %d\n\n", m.TotalUsers.Load())

	fmt.Fprintf(w, "# HELP livecdn_bandwidth_mbps Total bandwidth in Mbps\n")
	fmt.Fprintf(w, "# TYPE livecdn_bandwidth_mbps gauge\n")
	fmt.Fprintf(w, "livecdn_bandwidth_mbps %d\n\n", m.TotalBWMbps.Load())

	fmt.Fprintf(w, "# HELP livecdn_register_total Agent registrations\n")
	fmt.Fprintf(w, "# TYPE livecdn_register_total counter\n")
	fmt.Fprintf(w, "livecdn_register_total %d\n\n", m.RegisterTotal.Load())

	fmt.Fprintf(w, "# HELP livecdn_heartbeat_total Heartbeats received\n")
	fmt.Fprintf(w, "# TYPE livecdn_heartbeat_total counter\n")
	fmt.Fprintf(w, "livecdn_heartbeat_total %d\n\n", m.HeartbeatTotal.Load())

	fmt.Fprintf(w, "# HELP livecdn_quality_reports_total Quality reports received\n")
	fmt.Fprintf(w, "# TYPE livecdn_quality_reports_total counter\n")
	fmt.Fprintf(w, "livecdn_quality_reports_total %d\n", m.QualityReported.Load())
}

// IsNodeHealthy 公开版本，供 metrics 使用
func IsNodeHealthy(n *common.NodeInfo) bool {
	return common.IsNodeHealthy(n)
}
