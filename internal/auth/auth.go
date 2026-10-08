// Package auth 解析 TRAE SOLO auth 文件（嵌套形：auth + account），
// 提供原子写回与目录扫描。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt），
// 写路径（upstream.RefreshToken）持写锁整段执行 ExchangeToken；
// 读路径（NeedsRefresh/JWT/RefreshTokenValue）持读锁读取快照，
// 杜绝与写并发时的数据竞争（-race 实测确认）。
// 其余字段（Domain/ApiHost/MachineID/DeviceID/UID/...）加载后不变，直接读。
type Auth struct {
	mu sync.RWMutex

	AccessToken  string // Cloud-IDE-JWT 头用
	RefreshToken string // 每次 ExchangeToken 轮换
	ExpiresAt    int64  // Unix 秒（accessToken 过期时刻）
	Domain       string // "trae.cn"
	ApiHost      string // "https://api.trae.com.cn"（ExchangeToken host）
	MachineID    string // x-machine-id
	DeviceID     string // x-device-id（须为 15~16 位纯数字，见 IsRealDeviceID）
	MarketUserID string // x-market-user-id（客户端本地分配，随凭证持久化）
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 落盘路径；refresh 后原子写回
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加写锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的写锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// RLock 供读路径持有读锁（与写锁互斥，读读不互斥）。
func (a *Auth) RLock() { a.mu.RLock() }

// RUnlock 释放 a.RLock 获取的读锁。
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 accessToken 的读锁快照，防与 RefreshToken 写并发竞态。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// DeviceIDValue 返回当前 deviceID 的读锁快照（EnsureDeviceID 会改写该字段）。
func (a *Auth) DeviceIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DeviceID
}

// MarketUserIDValue 返回当前 marketUserId 的读锁快照。
func (a *Auth) MarketUserIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.MarketUserID
}

// IsRealDeviceID 报告 deviceID 是否为真实客户端形态（15~16 位纯数字）。
//
// 上游把 x-device-id 当设备指纹：真实客户端实测值形如 `1711320556112436`（16 位纯数字）。
// 发 hex32 / UUID 在风控眼里根本不是设备号，签到会被判无效并**稳定返回 9074**
// 「当前参与用户太多」（实测：换成 16 位数字后立即 code 0 签到成功）。
func IsRealDeviceID(id string) bool {
	if len(id) < 15 || len(id) > 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// EnsureDeviceID 确保 deviceID 为真实形态（15~16 位纯数字）。
// 未设置、或存的是历史遗留的 hex32/UUID，都会重新生成；返回是否本次改写
// （调用方据此决定是否 SaveAtomic 落盘）。
//
// 迁移安全性：上游判重维度是**账号**不是设备（换 deviceId 后 checked_in 仍为 true），
// 所以把存量凭证的 hex32 一次性迁到数字形态不会导致重复签到。
//
// 背景：x-device-id 缺失会返回 9004「The submitted order parameters are incorrect」；
// 形态不对（hex32/UUID）则稳定 9074（均实测）。
func (a *Auth) EnsureDeviceID() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if IsRealDeviceID(a.DeviceID) {
		return false
	}
	a.DeviceID = newDeviceID()
	return true
}

// EnsureMarketUserID 若未设置 marketUserId 则生成一个 uuid-v4；返回是否本次新生成。
//
// 该 id 服务端不提供（抓包所有响应体都没有它），由客户端本地为该账号分配并持久化。
// 每次请求现生成会破坏指纹稳定性——真实客户端对同一账号始终发同一个值。
func (a *Auth) EnsureMarketUserID() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.MarketUserID != "" {
		return false
	}
	a.MarketUserID = newMarketUserID()
	return true
}

// newDeviceID 生成 16 位纯数字设备号（首位 1-9，保证恰好 16 位）。
//
// 形态须对齐真实客户端。**不能用 `10^15 + rand` 那种写法**：那会把高位钉死在
// `1xxxxx`，批量生成的号共享可识别前缀，反而给风控递「同一生成器批发」的特征。
// 首位取 1-9（不能是 0，否则不是 16 位），后 15 位逐位均匀取 0-9。
func newDeviceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	out := make([]byte, 16)
	out[0] = '1' + (b[0] % 9) // '1'..'9'
	for i := 1; i < 16; i++ {
		out[i] = '0' + (b[i] % 10)
	}
	return string(out)
}

// newMarketUserID 生成 uuid-v4 形标识（8-4-4-4-12）。
func newMarketUserID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// RefreshTokenValue 返回当前 refreshToken 的读锁快照，防与 RefreshToken 写并发竞态。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本；调用方必须已持有 a.mu（读或写锁）。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// parseNested 兼容现有 trae-*.json 嵌套形：
//
//	{"account":{...},"auth":{...}}
func parseNested(raw []byte) (*Auth, error) {
	var n struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
			MarketUserID string `json:"marketUserId"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  n.Auth.AccessToken,
		RefreshToken: n.Auth.RefreshToken,
		ExpiresAt:    n.Auth.ExpiresAt,
		Domain:       n.Auth.Domain,
		ApiHost:      n.Auth.ApiHost,
		MachineID:    n.Auth.MachineID,
		DeviceID:     n.Auth.DeviceID,
		MarketUserID: n.Auth.MarketUserID,
		UID:          n.Account.UID,
		EnterpriseID: n.Account.EnterpriseID,
		Nickname:     n.Account.Nickname,
	}, nil
}

// parseFlat 兼容扁平形（CPA 面板手建等）：
//
//	{"accessToken":...,"uid":...,"machineId":...,"deviceId":...}
func parseFlat(raw []byte) (*Auth, error) {
	var f struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		ApiHost      string `json:"apiHost"`
		MachineID    string `json:"machineId"`
		DeviceID     string `json:"deviceId"`
		MarketUserID string `json:"marketUserId"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiresAt:    f.ExpiresAt,
		Domain:       f.Domain,
		ApiHost:      f.ApiHost,
		MachineID:    f.MachineID,
		DeviceID:     f.DeviceID,
		MarketUserID: f.MarketUserID,
		UID:          f.UID,
		EnterpriseID: f.EnterpriseID,
		Nickname:     f.Nickname,
	}, nil
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （登录脚本产出，现有 trae-*.json）
//	扁平形 {"accessToken":...,"uid":...}   （手建）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var (
		a   *Auth
		err error
	)
	if _, nested := probe["auth"]; nested {
		a, err = parseNested(raw)
	} else {
		a, err = parseFlat(raw)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename，0600），保持登录脚本可读格式。
// 加锁外壳：防止与 RefreshToken 并发读写 token 字段导致写回半更新。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

// CredentialsDoc 返回嵌套形凭证文档（{"auth":..,"account":..}），
// 与磁盘 trae-*.json 同构，可直接 json.Marshal 后喂给 Parse。
//
// 供账号导出接口 / 本地签到程序复用：两端共用同一序列化口径，
// 避免字段名漂移导致「导出的凭证本地解不开」。
func (a *Auth) CredentialsDoc() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.credentialsDocLocked()
}

// credentialsDocLocked 是 CredentialsDoc 的持锁内部版本；调用方必须已持有 a.mu。
func (a *Auth) credentialsDocLocked() map[string]any {
	return map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
			"marketUserId": a.MarketUserID,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
}

// saveAtomicLocked 是 SaveAtomic 的持锁内部版本；调用方必须已持有 a.mu。
func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := a.credentialsDocLocked()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// FilePathFor 在已知 AuthDir 时构造 trae-{uid}.json 落盘路径。
func FilePathFor(authDir, uid string) string {
	return filepath.Join(authDir, "trae-"+uid+".json")
}

// MaskToken 保留前 n 字符 + 省略号；不足则全显示。用于面板 JSON 预览脱敏。
func MaskToken(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(" + strconv.Itoa(len(s)) + " chars)"
}

// LoadDir 扫描 dir 下 trae-*.json。解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
