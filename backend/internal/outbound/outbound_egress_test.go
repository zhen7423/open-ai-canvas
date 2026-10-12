package outbound

import "testing"

func TestIsNonPublicHost(t *testing.T) {
	private := []string{"", "192.168.1.167", "10.0.0.5", "127.0.0.1", "::1", "localhost", "nas", "canvas.local", "intranet.internal", "host.home.arpa"}
	for _, host := range private {
		if !IsNonPublicHost(host) {
			t.Fatalf("IsNonPublicHost(%q) = false, want true", host)
		}
	}
	public := []string{"api.metaso.cn", "metaso.cn", "oss-cn-beijing.aliyuncs.com", "trycloudflare.com"}
	for _, host := range public {
		if IsNonPublicHost(host) {
			t.Fatalf("IsNonPublicHost(%q) = true, want false", host)
		}
	}
}

func TestIsNonPublicHostRejectsTrickyAddressShapes(t *testing.T) {
	// 这些都是 net.ParseIP 会拒绝、但某些解析器仍按内网地址处理，或本身就不该发给第三方的形态。
	// 判不准时按非公网处理：代价只是多做一次镜像，而不是把内部地址交给外部厂商。
	blocked := []string{
		"100.64.5.7",         // Tailscale/CGNAT 段，不在 net.IP.IsPrivate 覆盖内
		"198.18.0.1",         // 基准测试段
		"192.0.2.1",          // 文档示例段
		"0177.0.0.1",         // 八进制写法
		"0x7f.0.0.1",         // 十六进制写法
		"127.1",              // 少段简写
		"0.0.0.0",            // 未指定地址
		"a.localhost",        // *.localhost 由解析器指回本机
		"192.168.1.167:1888", // 带端口
	}
	for _, host := range blocked {
		if !IsNonPublicHost(host) {
			t.Fatalf("IsNonPublicHost(%q) = false, want true", host)
		}
	}
	allowed := []string{
		"metaso.cn",
		"api.metaso.cn",
		"oss-cn-beijing.aliyuncs.com",
		"1e100.net", // 首段含字母的真实公网域名，不该被数字段规则误伤
		"beef-cafe.example.com",
		"a0b1c2d3.trycloudflare.com",
	}
	for _, host := range allowed {
		if IsNonPublicHost(host) {
			t.Fatalf("IsNonPublicHost(%q) = true, want false", host)
		}
	}
}

// 地址字面量的判据只看"字符都属于 0-9a-f"时，会把每个标签恰好都能用十六进制字母拼出的真实域名
// 误判成内网形态：后果是出站内镜与内联对该渠道整条静默不生效（素材继续带内网地址交给上游），
// 管理员也存不下合法的 Endpoint。所以字面量必须是"十进制数字段"或"显式 0x 段"。
func TestIsNonPublicHostDoesNotTreatHexWordDomainsAsLiterals(t *testing.T) {
	public := []string{
		"facade.de", "deca.cafe", "cafe.de", "add.de", "abcdef.cn", "feed.beef.com",
	}
	for _, host := range public {
		if IsNonPublicHost(host) {
			t.Fatalf("IsNonPublicHost(%q) = true, want false：这是真实公网域名，不该被当地址字面量", host)
		}
	}
	// 收紧判据后仍要抓住真正的不规范 IP 写法，包括大写 0X。
	blocked := []string{"0x7f.0.0.1", "0X7F.0.0.1", "0177.0.0.1", "127.1", "10.0.0.1"}
	for _, host := range blocked {
		if !IsNonPublicHost(host) {
			t.Fatalf("IsNonPublicHost(%q) = false, want true：这是不该发给第三方的地址形态", host)
		}
	}
}
