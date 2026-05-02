package controller

import (
	"encoding/binary"
	"net"
	"os"
	"sync"
)

// IPLocator 提供客户端 IP → 地区/运营商 查询
//
// 重要：此库仅用于识别【观众客户端】的地理位置，
// 以便调度时优先分配同线路节点。
//
// 节点自身的 region/isp 在注册时由运维人员指定，
// 不走自动探测——因为 NAT 挂机宝的线路、机房是固定的，
// 只有运维知道这机器走的什么线。
//
// 使用场景：
//   观众 IP → IPLocator.Lookup() → "华东/电信"
//   → 调度器优先分配 region="华东" isp="电信" 的节点
type IPLocator struct {
	mu      sync.RWMutex
	entries []geoEntry
}

type geoEntry struct {
	cidr   *net.IPNet
	region string
	isp    string
}

func NewIPLocator() *IPLocator {
	loc := &IPLocator{}
	loc.loadDefaults()
	return loc
}

// LoadFromFile 从 ip2region 数据库文件加载 (可选)
// 如果放置了 ip2region.xdb 文件，精度会高很多
func (loc *IPLocator) LoadFromFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	// TODO: 解析 ip2region xdb 格式
	// 参考: github.com/lionsoul2014/ip2region/binding/golang
	return nil
}

// Lookup 根据【客户端】IP 查询地区和运营商
// 返回值仅用于调度辅助，不影响节点注册
func (loc *IPLocator) Lookup(ipStr string) (region, isp string) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "未知", "未知"
	}

	loc.mu.RLock()
	defer loc.mu.RUnlock()

	for _, e := range loc.entries {
		if e.cidr.Contains(ip) {
			return e.region, e.isp
		}
	}

	if isPrivateIP(ip) {
		return "内网", "内网"
	}

	return ipToChinaRegion(ip)
}

// 内置中国三大运营商的典型 IP 大段
// 精度有限，建议生产环境替换为 ip2region
func (loc *IPLocator) loadDefaults() {
	defaults := []struct {
		cidr   string
		region string
		isp    string
	}{
		// 电信
		{"58.0.0.0/8", "华东", "电信"},
		{"61.0.0.0/8", "华北", "电信"},
		{"116.0.0.0/8", "华南", "电信"},
		{"117.0.0.0/8", "华东", "电信"},
		{"118.0.0.0/8", "华南", "电信"},
		{"119.0.0.0/8", "华东", "电信"},
		{"180.0.0.0/8", "华北", "电信"},
		{"221.0.0.0/8", "华东", "电信"},
		// 移动
		{"111.0.0.0/8", "华东", "移动"},
		{"112.0.0.0/8", "华南", "移动"},
		{"120.0.0.0/8", "华北", "移动"},
		{"183.0.0.0/8", "华东", "移动"},
		{"223.0.0.0/8", "华南", "移动"},
		// 联通
		{"60.0.0.0/8", "华北", "联通"},
		{"106.0.0.0/8", "华东", "联通"},
		{"110.0.0.0/8", "华北", "联通"},
		{"123.0.0.0/8", "华北", "联通"},
		{"175.0.0.0/8", "华东", "联通"},
		{"202.0.0.0/8", "华北", "联通"},
		{"218.0.0.0/8", "华东", "联通"},
		{"220.0.0.0/8", "华北", "联通"},
	}

	loc.entries = make([]geoEntry, 0, len(defaults))
	for _, d := range defaults {
		_, cidr, err := net.ParseCIDR(d.cidr)
		if err == nil {
			loc.entries = append(loc.entries, geoEntry{cidr: cidr, region: d.region, isp: d.isp})
		}
	}
}

func isPrivateIP(ip net.IP) bool {
	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
	}
	for _, r := range privateRanges {
		_, cidr, _ := net.ParseCIDR(r)
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func ipToChinaRegion(ip net.IP) (string, string) {
	ip4 := ip.To4()
	if ip4 == nil {
		return "海外", "海外"
	}

	first := ip4[0]
	switch {
	case first == 58 || first == 61 || first == 116 || first == 117 || first == 180 || first == 221:
		return "中国", "电信"
	case first == 111 || first == 112 || first == 120 || first == 183 || first == 223:
		return "中国", "移动"
	case first == 60 || first == 106 || first == 110 || first == 123 || first == 220:
		return "中国", "联通"
	default:
		v := binary.BigEndian.Uint32(ip4)
		if v > 0x30000000 && v < 0xE0000000 {
			return "中国", "未知"
		}
		return "海外", "海外"
	}
}
