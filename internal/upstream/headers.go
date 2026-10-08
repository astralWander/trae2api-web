// headers.go SOLO 三类请求头：对话（SOLOHeaders）/ ug（UgHeaders）/ oauth（OAuthHeaders）。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"trae2api-web/internal/auth"
)

const clientUA = "Trae/" + IdeVersion

// SOLOHeaders 设置 llm_utils_chat / get_detail_param 所需的 SOLO 专属头。
// 规则来自 SPEC §1 SOLO headers（实测必须）。
func SOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA)
	at := a.JWT() // 读锁快照，防与 RefreshToken 写并发竞态
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// UgHeaders 设置签到/积分（api.trae.cn）所需头。
//
// 逐头对齐 2026-09-03 抓包的**成功**签到请求：伪装身份必须是 **VSCode 插件进程**
// （UA `VSCode 1.107.1 (TRAE SOLO CN)`），不是 IDE 主进程的 `Trae/{IdeVersion}`。
// 另外 X-Device-Id 必须存在且为真实形态（15~16 位数字），否则 9004/9074；
// 由调用方先 ensureIdentity 保证非空且合规。
func UgHeaders(req *http.Request, a *auth.Auth) {
	reqID := uuidV4()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*") // 实测值；不是 application/json
	req.Header.Set("User-Agent", UgUserAgent)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT()) // 读锁快照
	req.Header.Set("X-User-Region", "CN")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Package-Type", "stable_cn")
	req.Header.Set("X-Lgw-Req-Sdk-Type", "3")
	req.Header.Set("X-Market-Client-Id", MarketClientID)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("App-Version", UgAppVersion)
	req.Header.Set("X-Request-Id", reqID)
	req.Header.Set("X-TT-Trace-Id", ttTraceID(reqID))
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "no-cors") // 实测值；不是 cors
	req.Header.Set("Sec-Fetch-Site", "none")
	if dev := a.DeviceIDValue(); dev != "" {
		req.Header.Set("X-Device-Id", dev)
	}
	if mkt := a.MarketUserIDValue(); mkt != "" {
		req.Header.Set("X-Market-User-Id", mkt)
	}
}

// uuidV4 生成 uuid-v4（8-4-4-4-12），用作 X-Request-Id。
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ttTraceID 生成 tt-trace-id，格式 00-32hex-16hex-01（与真实客户端一致）。
func ttTraceID(reqID string) string {
	var tb [16]byte
	if _, err := rand.Read(tb[:]); err != nil {
		return ""
	}
	span := strings.ReplaceAll(reqID, "-", "")
	if len(span) > 16 {
		span = span[:16]
	}
	return "00-" + hex.EncodeToString(tb[:]) + "-" + span + "-01"
}

// OAuthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}
