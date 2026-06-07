//! Agent Mesh 级联选择器 V2 — 中心编排 + 边缘自治
//!
//! 核心架构:
//!   Controller: 负责"授权、约束和候选集" — 告诉 Agent 可以连谁、优先连谁、不能连谁
//!   Agent: 负责"本地探测、优选和故障切换" — 自己测现在连谁最快、最稳、最不拥塞
//!
//! 职责划分:
//!   Controller 决定:
//!     - candidate_upstreams: 你可以连谁 (候选集)
//!     - MeshPolicy: 切换策略约束 (冷却时间、最大次数等)
//!     - Tier 规则: 谁是你的上级, 禁止同层/反向
//!     - MigrationAdvice: 全局拓扑优化的迁移建议
//!
//!   Agent 决定:
//!     - 当前选谁: 从候选集中本地评分择优
//!     - 故障切换: 上游不可用时立即切换, 不等 Controller
//!     - Mesh Ping: 定时探测候选上游的实时延迟和质量
//!     - 迟滞控制: 防止等价路径间频繁切换
//!
//! 参考:
//!   - Tailscale: betterAddr hysteresis + Disco Ping/Pong
//!   - Envoy: Outlier Detection + 主动健康检查
//!   - Nginx: 被动健康检查 + effective_weight 慢恢复

use serde::Deserialize;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::RwLock;
use tracing::{debug, info, warn};

// --- Controller 下发的数据结构 ---

/// 候选上游 (与 Go 端 CandidateUpstream 对应)
#[derive(Debug, Clone, Deserialize)]
pub struct CandidateUpstream {
    pub node_id: String,
    pub url: String,
    pub region: String,
    pub isp: String,
    pub tier: i32,
    #[serde(default)]
    pub score_hint: f64,
    #[serde(default)]
    pub max_children: i32,
    #[serde(default)]
    pub current_children: i32,
    #[serde(default)]
    pub bw_remaining_mbps: f64,
    #[serde(default)]
    pub allow_cascade: bool,
    #[serde(default)]
    pub has_ipv6: bool,
    #[serde(default)]
    pub ipv6_url: String,
}

/// Origin 回退配置
#[derive(Debug, Clone, Deserialize)]
pub struct OriginFallback {
    pub url: String,
    pub priority: i32,
}

/// Controller 下发的切换策略
#[derive(Debug, Clone, Deserialize)]
pub struct MeshPolicy {
    #[serde(default = "default_max_depth")]
    pub max_depth: i32,
    #[serde(default = "default_switch_cooldown")]
    pub switch_cooldown_sec: i32,
    #[serde(default = "default_min_score_improvement")]
    pub min_score_improvement: i32,
    #[serde(default = "default_mesh_ping_interval")]
    pub mesh_ping_interval_sec: i32,
    #[serde(default = "default_candidate_ttl")]
    pub candidate_ttl_sec: i32,
    #[serde(default = "default_max_switch")]
    pub max_switch_per_5min: i32,
    #[serde(default = "default_hard_fail_timeout")]
    pub hard_fail_timeout_ms: i32,
}

fn default_max_depth() -> i32 { 2 }
fn default_switch_cooldown() -> i32 { 30 }
fn default_min_score_improvement() -> i32 { 15 }
fn default_mesh_ping_interval() -> i32 { 10 }
fn default_candidate_ttl() -> i32 { 60 }
fn default_max_switch() -> i32 { 3 }
fn default_hard_fail_timeout() -> i32 { 3000 }

/// 迁移建议
#[derive(Debug, Clone, Deserialize)]
pub struct MigrationAdvice {
    #[serde(default)]
    pub should_switch: bool,
    #[serde(default)]
    pub preferred_node: String,
    #[serde(default)]
    pub reason: String,
}

/// Mesh 拓扑响应 (与 Go 端 MeshMapResponse 对应)
#[derive(Debug, Clone, Deserialize)]
pub struct MeshMapResponse {
    pub mesh_epoch: u64,
    pub self_node_id: String,
    #[serde(default)]
    pub self_tier: i32,
    #[serde(default)]
    pub candidate_upstreams: Vec<CandidateUpstream>,
    #[serde(default)]
    pub origin_fallback: Option<OriginFallback>,
    #[serde(default)]
    pub policy: Option<MeshPolicy>,
    pub migration_advice: Option<MigrationAdvice>,
}

// --- Agent 本地数据结构 ---

/// 级联路径 (Agent 选择后的结果)
#[derive(Debug, Clone)]
pub struct CascadePath {
    pub upstream_url: String,
    pub upstream_node_id: String,
    pub score: f64,
    pub tier: i32,
    pub reason: String,
    pub selected_at: Instant,
}

/// 环路防护器
/// 检测级联路径中是否包含自己, 防止环路
#[derive(Debug, Clone)]
pub struct LoopGuard {
    /// 本节点 ID
    node_id: String,
    /// 已知的上游链路 (从心跳或协议帧中收集)
    known_upstream_chain: Vec<String>,
    /// 最大允许深度
    max_depth: i32,
}

impl LoopGuard {
    pub fn new(node_id: String, max_depth: i32) -> Self {
        Self {
            node_id,
            known_upstream_chain: Vec::new(),
            max_depth,
        }
    }

    /// 检查候选上游是否会导致环路
    /// 如果上游的上游链中包含自己, 则会形成环路
    pub fn would_create_loop(&self, upstream_node_id: &str, upstream_tier: i32) -> bool {
        // 如果上游的 tier >= 本节点的 tier, 禁止拉流 (同层或反向)
        // 这是 Tier 规则: 只能从更低层级拉流
        if upstream_tier >= 0 && upstream_tier as i32 >= self.max_depth {
            return true;
        }

        // 检查上游链中是否包含自己
        if upstream_node_id == self.node_id {
            return true;
        }

        if self.known_upstream_chain.contains(&self.node_id) {
            return true;
        }

        false
    }

    /// 记录上游链 (从协议帧的 path 字段解析)
    pub fn update_upstream_chain(&mut self, path: &[String]) {
        self.known_upstream_chain = path.to_vec();
    }

    /// 检查路径中是否包含自己 (实时检测)
    pub fn detect_loop_in_path(&self, path: &[String]) -> bool {
        path.contains(&self.node_id)
    }
}

/// 级联选择器 V2
/// 中心编排 + 边缘自治: Controller 下发候选上游和约束 → Agent 本地探测、优选、故障切换
pub struct CascadeSelector {
    /// 本节点 ID
    node_id: String,
    /// 本节点地域
    region: String,
    /// 本节点 ISP
    isp: String,
    /// 本节点 IPv6 可用性
    has_ipv6: bool,

    /// 当前选择的最优路径
    current_path: Arc<RwLock<Option<CascadePath>>>,
    /// 最近一次收到的 mesh_map_response
    last_mesh_response: Arc<RwLock<Option<MeshMapResponse>>>,
    /// 环路防护
    loop_guard: Arc<RwLock<LoopGuard>>,

    // --- 迟滞控制状态 ---
    /// 最近一次切换时间
    last_switch_at: Arc<RwLock<Option<Instant>>>,
    /// 5分钟内的切换次数
    switch_count_5min: Arc<RwLock<Vec<Instant>>>,
    /// 当前层级
    self_tier: Arc<RwLock<i32>>,

    // --- 边缘自治: 本地探测和健康监测 ---
    /// 候选上游的本地探测结果 (node_id → ProbeResult)
    probe_results: Arc<RwLock<std::collections::HashMap<String, ProbeResult>>>,
    /// 当前上游的连续失败计数 (Agent 本地异常检测, 参考 Envoy Outlier Detection)
    upstream_consecutive_fails: Arc<RwLock<i32>>,
    /// 当前上游最后成功时间
    upstream_last_ok_at: Arc<RwLock<Option<Instant>>>,
}

/// 本地探测结果 (Agent 自己测的, Controller 不知道)
#[derive(Debug, Clone)]
pub struct ProbeResult {
    /// TCP 连接 RTT (ms)
    pub connect_rtt_ms: u32,
    /// 探测时间
    pub probed_at: Instant,
    /// 是否可达
    pub reachable: bool,
}

impl CascadeSelector {
    pub fn new(node_id: String, region: String, isp: String, has_ipv6: bool) -> Self {
        Self {
            node_id: node_id.clone(),
            region,
            isp,
            has_ipv6,
            current_path: Arc::new(RwLock::new(None)),
            last_mesh_response: Arc::new(RwLock::new(None)),
            loop_guard: Arc::new(RwLock::new(LoopGuard::new(node_id, 2))),
            last_switch_at: Arc::new(RwLock::new(None)),
            switch_count_5min: Arc::new(RwLock::new(Vec::new())),
            self_tier: Arc::new(RwLock::new(2)),
            probe_results: Arc::new(RwLock::new(std::collections::HashMap::new())),
            upstream_consecutive_fails: Arc::new(RwLock::new(0)),
            upstream_last_ok_at: Arc::new(RwLock::new(None)),
        }
    }

    /// 更新 MeshMap (从心跳响应获取) 并重新评估路径
    /// 类似 Tailscale Conn.SetNetworkMap()
    pub async fn update_mesh_map(&self, resp: MeshMapResponse) {
        // 更新层级
        *self.self_tier.write().await = resp.self_tier;
        self.loop_guard.write().await.max_depth = resp.self_tier;

        // 更新策略中的 max_depth
        if let Some(ref policy) = resp.policy {
            self.loop_guard.write().await.max_depth = policy.max_depth;
        }

        // 处理 Controller 迁移建议
        if let Some(ref advice) = resp.migration_advice {
            if advice.should_switch && advice.reason == "upstream_offline" {
                // 上游已下线, 立即切换
                info!(
                    reason = &advice.reason,
                    "[CascadeSelector] Controller建议立即切换 (上游下线)"
                );
                self.force_reselect(&resp).await;
                *self.last_mesh_response.write().await = Some(resp);
                return;
            }
        }

        // 在候选列表中选择最优路径
        let new_path = self.select_from_candidates(&resp.candidate_upstreams, &resp);

        // 应用迟滞控制
        let should_switch = self.check_hysteresis(&new_path, &resp).await;

        if should_switch {
            self.apply_path_switch(new_path).await;
        }

        *self.last_mesh_response.write().await = Some(resp);
    }

    /// 从候选上游中选择最优路径
    /// Agent 只在 Controller 允许的候选中做本地择优
    fn select_from_candidates(
        &self,
        candidates: &[CandidateUpstream],
        _resp: &MeshMapResponse,
    ) -> Option<CascadePath> {
        if candidates.is_empty() {
            return None;
        }

        let mut scored: Vec<CascadePath> = Vec::new();

        for c in candidates {
            if !c.allow_cascade {
                continue;
            }
            if c.url.is_empty() {
                continue;
            }

            // 环路检测: tier 规则
            if let Ok(guard) = self.loop_guard.try_read() {
                if guard.would_create_loop(&c.node_id, c.tier) {
                    warn!(
                        upstream = &c.node_id,
                        tier = c.tier,
                        "[LoopGuard] 跳过可能导致环路的候选上游"
                    );
                    continue;
                }
            }

            // 综合评分: Controller hint (locality+isp+capacity+stability) + Agent本地探测
            // Controller 给了 score_hint (0~100), Agent 加本地 Mesh Ping 延迟修正
            let mut local_score = self.compute_local_score(c);

            // 本地 Mesh Ping 延迟修正 (边缘自治核心)
            // 如果有本地探测结果, 用 RTT 调整评分: RTT 越低分越高
            if let Some(rtt) = self.get_probe_rtt_sync(&c.node_id) {
                if rtt < 10 {
                    local_score *= 1.2; // 极低延迟 (<10ms), 大幅加分
                } else if rtt < 50 {
                    local_score *= 1.1; // 低延迟 (<50ms), 加分
                } else if rtt > 200 {
                    local_score *= 0.7; // 高延迟 (>200ms), 降分
                } else if rtt > 100 {
                    local_score *= 0.85; // 较高延迟 (>100ms), 降分
                }
            }

            let final_score = (c.score_hint * 0.7 + local_score * 100.0 * 0.3) / 100.0;

            let mut reason = if c.region == self.region && c.isp == self.isp {
                "同区同ISP".to_string()
            } else if c.region == self.region {
                "同区".to_string()
            } else {
                "跨区".to_string()
            };

            // IPv6 优先
            let upstream_url = if self.has_ipv6 && !c.ipv6_url.is_empty() {
                reason.push_str("+IPv6");
                c.ipv6_url.clone()
            } else {
                c.url.clone()
            };

            scored.push(CascadePath {
                upstream_url,
                upstream_node_id: c.node_id.clone(),
                score: final_score,
                tier: c.tier,
                reason,
                selected_at: Instant::now(),
            });
        }

        if scored.is_empty() {
            return None;
        }

        // 按分数降序
        scored.sort_by(|a, b| b.score.partial_cmp(&a.score).unwrap_or(std::cmp::Ordering::Equal));
        Some(scored.into_iter().next().unwrap())
    }

    /// Agent 本地评分 (补充 Controller 没有的实时信息)
    /// 参考 Tailscale betterAddr: 综合考虑容量、带宽、拓扑偏好
    fn compute_local_score(&self, candidate: &CandidateUpstream) -> f64 {
        let mut score = 1.0; // 基础分

        // 容量因子: 剩余下游数 / 剩余带宽
        if candidate.max_children > 0 {
            let children_avail = 1.0 - (candidate.current_children as f64 / candidate.max_children as f64);
            score *= 0.5 + 0.5 * children_avail;
        }

        if candidate.bw_remaining_mbps < 4.0 {
            score *= 0.7; // 带宽紧张, 降分
        }

        // 拓扑偏好 (参考 Tailscale betterAddr 网络加分)
        // Tier1 优先 (直连 Origin 更稳定)
        if candidate.tier == 1 {
            score *= 1.1;
        }

        // 同区同 ISP 加分 (类似 Tailscale Private IP)
        if candidate.region == self.region && candidate.isp == self.isp {
            score *= 1.15;
        } else if candidate.region == self.region {
            score *= 1.05;
        }

        // IPv6 加分 (类似 Tailscale IPv6 +10)
        if self.has_ipv6 && candidate.has_ipv6 {
            score *= 1.05;
        }

        score
    }

    /// 迟滞控制检查
    /// 参考 Tailscale hysteresis + 用户建议的5条规则:
    ///   1. 当前上游仍可用时, 不轻易切换
    ///   2. 新上游分数必须明显高于当前上游
    ///   3. 两次切换之间必须有冷却时间
    ///   4. 5分钟内切换次数有限制
    ///   5. 上游硬故障时快速切换 (不受冷却限制)
    async fn check_hysteresis(&self, new_path: &Option<CascadePath>, resp: &MeshMapResponse) -> bool {
        let current = self.current_path.read().await;

        match (&*current, new_path) {
            (None, None) => false,
            (None, Some(_)) => true,  // Origin → P2P
            (Some(_), None) => true,  // P2P → Origin (上游可能挂了)
            (Some(cur), Some(new)) => {
                // 同一个上游, 不切换
                if cur.upstream_node_id == new.upstream_node_id {
                    return false;
                }

                // 检查冷却时间
                let cooldown_sec = resp.policy.as_ref()
                    .map(|p| p.switch_cooldown_sec)
                    .unwrap_or(30);
                if let Some(last) = *self.last_switch_at.read().await {
                    if last.elapsed() < Duration::from_secs(cooldown_sec as u64) {
                        debug!("[CascadeSelector] 冷却期内, 不切换");
                        return false;
                    }
                }

                // 检查5分钟内切换次数
                let max_switch = resp.policy.as_ref()
                    .map(|p| p.max_switch_per_5min)
                    .unwrap_or(3);
                let mut switch_times = self.switch_count_5min.write().await;
                let cutoff = Instant::now() - Duration::from_secs(300);
                switch_times.retain(|t| *t > cutoff);
                if switch_times.len() >= max_switch as usize {
                    debug!(
                        count = switch_times.len(),
                        max = max_switch,
                        "[CascadeSelector] 5分钟内切换次数超限"
                    );
                    return false;
                }

                // 检查最小改善分数
                let min_improvement = resp.policy.as_ref()
                    .map(|p| p.min_score_improvement as f64 / 100.0)
                    .unwrap_or(0.15);
                let improvement = (new.score - cur.score) / cur.score.max(0.01);
                if improvement < min_improvement {
                    debug!(
                        improvement = format!("{:.1}%", improvement * 100.0),
                        required = format!("{:.1}%", min_improvement * 100.0),
                        "[CascadeSelector] 改善不足, 不切换"
                    );
                    return false;
                }

                true
            }
        }
    }

    /// 强制重新选择 (上游下线等场景, 不受迟滞限制)
    async fn force_reselect(&self, resp: &MeshMapResponse) {
        let new_path = self.select_from_candidates(&resp.candidate_upstreams, resp);
        self.apply_path_switch(new_path).await;
    }

    /// 应用路径切换
    async fn apply_path_switch(&self, new_path: Option<CascadePath>) {
        if let Some(ref path) = new_path {
            info!(
                upstream = &path.upstream_node_id[..8.min(path.upstream_node_id.len())],
                score = format!("{:.2}", path.score),
                tier = path.tier,
                reason = &path.reason,
                "[CascadeSelector] 路径切换"
            );
        } else {
            info!("[CascadeSelector] 回退到直连 Origin");
        }

        *self.current_path.write().await = new_path;
        *self.last_switch_at.write().await = Some(Instant::now());

        let mut switch_times = self.switch_count_5min.write().await;
        switch_times.push(Instant::now());
    }

    /// 获取当前最优上游的 FLV URL
    pub async fn current_upstream_url(&self) -> Option<String> {
        self.current_path.read().await.as_ref().map(|p| p.upstream_url.clone())
    }

    /// 获取当前路径信息
    pub async fn current_path(&self) -> Option<CascadePath> {
        self.current_path.read().await.clone()
    }

    /// 获取当前层级
    pub async fn self_tier(&self) -> i32 {
        *self.self_tier.read().await
    }

    /// 获取级联统计 (用于心跳上报)
    pub async fn cascade_stats(&self) -> CascadeStats {
        let path = self.current_path.read().await;
        let tier = *self.self_tier.read().await;

        CascadeStats {
            cascade_depth: if path.is_some() { tier - 1 } else { 0 },
            current_upstream: path.as_ref().map(|p| p.upstream_node_id.clone()).unwrap_or_default(),
            self_tier: tier,
        }
    }

    // ========== 边缘自治: 本地探测和故障切换 ==========

    /// 报告上游拉流成功 (由 FlvPuller 调用)
    /// Agent 自己发现上游正常 → 重置失败计数
    pub async fn report_upstream_ok(&self) {
        *self.upstream_consecutive_fails.write().await = 0;
        *self.upstream_last_ok_at.write().await = Some(Instant::now());
    }

    /// 报告上游拉流失败 (由 FlvPuller 调用)
    /// Agent 自己发现上游异常 → 本地决策是否切换
    /// 参考 Envoy Outlier Detection: 连续失败超过阈值 → 立即切换, 不等 Controller
    pub async fn report_upstream_fail(&self) {
        let mut fails = self.upstream_consecutive_fails.write().await;
        *fails += 1;

        let consecutive_fails = *fails;
        drop(fails);

        // 连续失败超过 3 次 (约 9-15s, 取决于重连间隔) → 判定硬故障, 立即切换
        // 参考 Envoy consecutive_5xx 触发驱逐
        if consecutive_fails >= 3 {
            warn!(
                fails = consecutive_fails,
                "[CascadeSelector] 上游连续失败{}次, 判定硬故障, 立即切换",
                consecutive_fails
            );
            self.emergency_switch().await;
        }
    }

    /// 紧急切换: 上游硬故障时立即切换, 不受迟滞控制限制
    /// 这是"边缘自治"的核心 — Agent 不等 Controller, 自己决定切换
    async fn emergency_switch(&self) {
        let resp = self.last_mesh_response.read().await.clone();
        if let Some(resp) = resp {
            self.force_reselect(&resp).await;
        } else {
            // 没有候选列表 → 回退 Origin
            *self.current_path.write().await = None;
            info!("[CascadeSelector] 紧急切换: 无候选, 回退直连 Origin");
        }
        *self.upstream_consecutive_fails.write().await = 0;
    }

    /// 执行 Mesh Ping: 探测所有候选上游的实时延迟
    /// Agent 本地自治探测, 不依赖 Controller
    /// 参考 Tailscale Disco Ping/Pong + Envoy 主动健康检查
    pub async fn run_mesh_ping(&self) {
        let resp = self.last_mesh_response.read().await.clone();
        if resp.is_none() {
            return;
        }
        let resp = resp.unwrap();

        for candidate in &resp.candidate_upstreams {
            if !candidate.allow_cascade || candidate.url.is_empty() {
                continue;
            }

            let rtt = Self::mesh_ping(&candidate.url).await;
            let reachable = rtt < 9999;

            let result = ProbeResult {
                connect_rtt_ms: rtt,
                probed_at: Instant::now(),
                reachable,
            };

            self.probe_results.write().await.insert(candidate.node_id.clone(), result);
        }

        // 如果当前上游探测不可达 → 主动切换 (不等心跳)
        let current_upstream_id = {
            let current = self.current_path.read().await;
            current.as_ref().map(|p| p.upstream_node_id.clone())
        };
        if let Some(ref upstream_id) = current_upstream_id {
            let probes = self.probe_results.read().await;
            if let Some(probe) = probes.get(upstream_id) {
                // 探测结果超过 30 秒 → 过期, 不做判断
                if probe.probed_at.elapsed() < Duration::from_secs(30) && !probe.reachable {
                    drop(probes);
                    warn!(
                        upstream = upstream_id,
                        "[CascadeSelector] Mesh Ping 发现上游不可达, 主动切换"
                    );
                    self.emergency_switch().await;
                }
            }
        }
    }

    /// 获取本地探测结果 (用于本地评分修正)
    pub async fn get_probe_rtt(&self, node_id: &str) -> Option<u32> {
        let probes = self.probe_results.read().await;
        probes.get(node_id).filter(|p| p.probed_at.elapsed() < Duration::from_secs(60)).map(|p| p.connect_rtt_ms)
    }

    /// 同步版本: 获取本地探测 RTT (用于 select_from_candidates)
    fn get_probe_rtt_sync(&self, node_id: &str) -> Option<u32> {
        match self.probe_results.try_read() {
            Ok(probes) => probes.get(node_id)
                .filter(|p| p.probed_at.elapsed() < Duration::from_secs(60))
                .map(|p| p.connect_rtt_ms),
            Err(_) => None, // 锁竞争时跳过, 不影响评分
        }
    }

    /// 环路检测: 检查路径中是否包含自己
    pub async fn detect_loop(&self, path: &[String]) -> bool {
        self.loop_guard.read().await.detect_loop_in_path(path)
    }

    /// 测量到候选上游的 RTT (Mesh Ping)
    pub async fn mesh_ping(url: &str) -> u32 {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap_or_default();

        let start = Instant::now();
        match client.head(url).send().await {
            Ok(resp) if resp.status().is_success() => {
                start.elapsed().as_millis() as u32
            }
            _ => 9999,
        }
    }
}

/// 级联统计 (用于心跳上报)
#[derive(Debug, Clone)]
pub struct CascadeStats {
    pub cascade_depth: i32,
    pub current_upstream: String,
    pub self_tier: i32,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_candidate(id: &str, region: &str, isp: &str, tier: i32, score: f64, allow: bool) -> CandidateUpstream {
        CandidateUpstream {
            node_id: id.into(),
            url: format!("http://10.0.0.{}:9090/cascade/test.flv", &id[5..]),
            region: region.into(),
            isp: isp.into(),
            tier,
            score_hint: score,
            max_children: 8,
            current_children: 0,
            bw_remaining_mbps: 30.0,
            allow_cascade: allow,
            has_ipv6: false,
            ipv6_url: String::new(),
        }
    }

    fn make_mesh_response(candidates: Vec<CandidateUpstream>) -> MeshMapResponse {
        MeshMapResponse {
            mesh_epoch: 1,
            self_node_id: "test-node".into(),
            self_tier: 2,
            candidate_upstreams: candidates,
            origin_fallback: Some(OriginFallback { url: "http://origin/live/test.flv".into(), priority: 999 }),
            policy: Some(MeshPolicy {
                max_depth: 2,
                switch_cooldown_sec: 30,
                min_score_improvement: 15,
                mesh_ping_interval_sec: 10,
                candidate_ttl_sec: 60,
                max_switch_per_5min: 3,
                hard_fail_timeout_ms: 3000,
            }),
            migration_advice: None,
        }
    }

    #[tokio::test]
    async fn test_select_from_candidates_same_region_isp() {
        let selector = CascadeSelector::new("test-node".into(), "华东".into(), "电信".into(), false);

        let candidates = vec![
            make_candidate("edge-1", "华东", "电信", 1, 90.0, true),
            make_candidate("edge-2", "华东", "联通", 1, 80.0, true),
            make_candidate("edge-3", "华北", "电信", 1, 70.0, true),
        ];

        let resp = make_mesh_response(candidates);
        let path = selector.select_from_candidates(&resp.candidate_upstreams, &resp);
        assert!(path.is_some());
        assert_eq!(path.unwrap().upstream_node_id, "edge-1");
    }

    #[tokio::test]
    async fn test_no_candidates_returns_none() {
        let selector = CascadeSelector::new("test-node".into(), "华东".into(), "电信".into(), false);

        let resp = make_mesh_response(vec![]);
        let path = selector.select_from_candidates(&resp.candidate_upstreams, &resp);
        assert!(path.is_none());
    }

    #[tokio::test]
    async fn test_loop_guard_prevents_same_tier() {
        let mut guard = LoopGuard::new("node-A".into(), 2);
        guard.max_depth = 2;

        // Tier1 上游可以被 Tier2 节点选择
        assert!(!guard.would_create_loop("node-B", 1));
        // 自己不能选自己
        assert!(guard.would_create_loop("node-A", 1));
    }

    #[tokio::test]
    async fn test_loop_guard_detects_loop_in_path() {
        let guard = LoopGuard::new("node-A".into(), 2);

        let path_without = vec!["origin".into(), "node-B".into(), "node-C".into()];
        assert!(!guard.detect_loop_in_path(&path_without));

        let path_with = vec!["origin".into(), "node-B".into(), "node-A".into()];
        assert!(guard.detect_loop_in_path(&path_with));
    }

    #[tokio::test]
    async fn test_hysteresis_cooldown() {
        let selector = CascadeSelector::new("test-node".into(), "华东".into(), "电信".into(), false);

        // 初始选择
        let resp1 = make_mesh_response(vec![
            make_candidate("edge-1", "华东", "电信", 1, 85.0, true),
        ]);
        selector.update_mesh_map(resp1).await;
        assert_eq!(selector.current_path().await.unwrap().upstream_node_id, "edge-1");

        // 短时间内尝试切换 (冷却期, 不应切换)
        let resp2 = make_mesh_response(vec![
            make_candidate("edge-1", "华东", "电信", 1, 85.0, true),
            make_candidate("edge-2", "华东", "电信", 1, 99.0, true), // 明显更好
        ]);
        selector.update_mesh_map(resp2).await;

        // 冷却期内, 应保持在 edge-1
        let current = selector.current_path().await;
        assert!(current.is_some());
        // 仍在冷却期, 不应切换
        assert_eq!(current.unwrap().upstream_node_id, "edge-1");
    }

    #[tokio::test]
    async fn test_cascade_stats() {
        let selector = CascadeSelector::new("test-node".into(), "华东".into(), "电信".into(), false);

        let resp = make_mesh_response(vec![
            make_candidate("edge-1", "华东", "电信", 1, 90.0, true),
        ]);
        selector.update_mesh_map(resp).await;

        let stats = selector.cascade_stats().await;
        assert_eq!(stats.self_tier, 2);
        assert_eq!(stats.current_upstream, "edge-1");
    }

    #[tokio::test]
    async fn test_ipv6_preference_in_candidates() {
        let selector = CascadeSelector::new("test-node".into(), "华东".into(), "电信".into(), true);

        let mut c = make_candidate("edge-1", "华东", "电信", 1, 90.0, true);
        c.has_ipv6 = true;
        c.ipv6_url = "http://[2001:db8::1]:9090/cascade/test.flv".into();

        let resp = make_mesh_response(vec![c]);
        let path = selector.select_from_candidates(&resp.candidate_upstreams, &resp);
        assert!(path.is_some());
        assert!(path.unwrap().upstream_url.contains("2001"));
    }
}
