// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"trae2api-web/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrPlanLimit                  // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                   // 429 → 短冷却 60s
	ErrSessionDead                // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                   // 404 → 短冷却 60s 不累计 errCount
	ErrServer                     // 5xx
	ErrClient                     // 其他 4xx
	ErrCheckinDenied              // 9074 签到专属拒绝（HTTP 200 业务码）→ 可单次快速重试
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrCheckinDenied:
		return "checkin_denied"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。Code 为业务错误码（HTTP 200 响应体里的 code），无则为 0。
type Error struct {
	Kind   ErrKind
	Status int
	Code   int64
	Msg    string
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("upstream %s (http %d, code %d): %s", e.Kind, e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 1005 plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	// session 失效
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// ClassifyBusiness 按响应体里的业务 code 分类（TRAE 惯例：HTTP 200 + {"code":N}）。
// code == 0 视为成功；未知非 0 归 ErrClient。
func ClassifyBusiness(code int64) ErrKind {
	switch code {
	case 0:
		return ErrNone
	case 1001:
		return ErrSessionDead
	case 1005, 4008:
		return ErrPlanLimit
	case 4011:
		// 请求频率超限 → 短冷却
		return ErrSoftRate
	case 9074:
		// 签到专属拒绝，与 chat 限流无关，单列一类以免污染限流计数
		return ErrCheckinDenied
	default:
		return ErrClient
	}
}

// businessCode 从响应体提取业务 code/message。无 code 字段（如纯数据响应）返回 0。
// 兼容 message / msg 两种字段名。
func businessCode(raw json.RawMessage) (int64, string) {
	var env struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Code == nil {
		return 0, ""
	}
	msg := env.Message
	if msg == "" {
		msg = env.Msg
	}
	return *env.Code, msg
}

// Client SOLO 上游 HTTP 客户端。Host 字段可覆盖便于测试。
type Client struct {
	// HTTP 用于短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底「上游一直不返回首字节」的悬挂。
	// 与 HTTP 共享同一 Transport（连接池复用）。nil 时 ChatStream 回退 HTTP。
	StreamHTTP *http.Client

	AgentHost string // https://trae-api-cn.mchost.guru
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	// 业务错误：HTTP 200 但响应体 code != 0（TRAE UG 接口惯例，如 9074 签到人数过多）。
	// 不校验会导致 CheckinClaim 把业务失败当成功、CheckinStatus 解析出全零误判为 "未开放"。
	if code, msg := businessCode(raw); code != 0 {
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return nil, &Error{Kind: ClassifyBusiness(code), Status: resp.StatusCode, Code: code, Msg: msg}
	}
	return raw, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换）。
// 成功时更新 a 的字段；调用方负责 SaveAtomic。全程持 a 写锁。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期（或已过期）时才刷新，
// 返回是否真正刷新。持锁内重查，避免并发请求对同一账号重复 ExchangeToken 轮换。
// 调用方仅在 returned 为 true 时需要 SaveAtomic。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a 写锁。
// 任何失败路径都不改写 a 字段，保证旧 refreshToken 可重试。
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，需归一化为 Unix 秒）
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
// 上游返回毫秒（如 1786847930141），auth 文件用秒（1786847930）。
// 毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	// 用专用流客户端（无总超时），避免长 SSE 流被 HTTP.Timeout 截断。
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64 // = maxInputTokens
	MaxTokens     int64 // = maxOutputTokens
}

// FetchModels 拉 SOLO 模型表（get_detail_param，32 配置）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
			ModelDetailList []struct {
				ModelName string `json:"model_name"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:   cfg.ConfigName,
			Name: cfg.DisplayConfig.DisplayName,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// CheckinResult 一次签到流程的结果。
type CheckinResult int

const (
	CheckinDone     CheckinResult = iota // 本次签到成功
	CheckinAlready                       // 今日已签到
	CheckinDisabled                      // 签到未开放（enable=false）
)

// String 便于日志/接口输出。
func (r CheckinResult) String() string {
	switch r {
	case CheckinDone:
		return "ok"
	case CheckinAlready:
		return "already"
	case CheckinDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// checkinRetryDelays 9074「签到专属拒绝」的重试间隔。
//
// 上游 9074 是**账号级稳定拒绝**（实测：同账号 40 余次请求全 9074，换 deviceId/token/
// UA/region/body 均无效），不是短时抖动。保留一次快速重试只为兜「万一上游恢复成真
// 抖动」，落空即判失败，不空耗 8s。变量形式便于测试缩短。
var checkinRetryDelays = []time.Duration{time.Second}

// checkinAlreadyCode 今日已签到（幂等成功，不算失败）。
const checkinAlreadyCode = 9095

// Checkin 执行一次完整签到：查状态 → 未签且开放则 claim → 回查确认。
// 遇到 9074（签到专属拒绝）按 checkinRetryDelays 单次快速重试。
// 返回的 error 仅在真正失败（网络/业务错误）时非 nil；已签到/未开放走 result。
func (c *Client) Checkin(a *auth.Auth) (CheckinResult, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		res, err := c.checkinOnce(a)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// 9074 换设备号无效（失败跟账号走），只做一次快速重试。
		var ue *Error
		if errors.As(err, &ue) && ue.Kind == ErrCheckinDenied && attempt < len(checkinRetryDelays) {
			time.Sleep(checkinRetryDelays[attempt])
			continue
		}
		return res, lastErr
	}
}

// checkinOnce 单次签到（不重试）。
//
// 判定规则（对齐上游语义）：
//   - 只看 HTTP 状态会误报成功（上游一律 200，成败藏在 body code）
//   - claim 拿到 code 0 也要**回查 status.checked_in** 才算数
//   - 已签到（status.checked_in=true 或 claim 返回 9095）是幂等成功
func (c *Client) checkinOnce(a *auth.Auth) (CheckinResult, error) {
	checkedIn, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return CheckinDisabled, err
	}
	if checkedIn {
		return CheckinAlready, nil
	}
	if !enable {
		return CheckinDisabled, nil
	}
	claimed, err := c.CheckinClaim(a)
	if err != nil {
		return CheckinDisabled, err
	}
	if claimed == CheckinAlready {
		return CheckinAlready, nil
	}
	// claim 返回 code 0：回查确认，上游确实标记 checked_in 才算签到成功。
	after, _, _, err := c.CheckinStatus(a)
	if err != nil {
		return CheckinDisabled, err
	}
	if !after {
		return CheckinDisabled, fmt.Errorf("checkin not effective: upstream did not mark checked_in")
	}
	return CheckinDone, nil
}

// checkinReqBody 签到接口请求体。真实客户端 status/claim 均发空对象（2026-09-03 抓包实测）。
var checkinReqBody = []byte(`{}`)

// ensureIdentity 确保账号具备完整的 ug 请求身份：真实形态 deviceId（15~16 位数字）
// + marketUserId（uuid-v4）。必要时生成并落盘。
//
// deviceId 形态是签到成败的关键：hex32/UUID 会被风控判为无效设备 → 稳定 9074；
// 缺失则 9004。两者都是实测踩过的坑。
func (c *Client) ensureIdentity(a *auth.Auth) {
	changed := a.EnsureDeviceID()
	if a.EnsureMarketUserID() {
		changed = true
	}
	if changed && a.FilePath != "" {
		_ = a.SaveAtomic()
	}
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	c.ensureIdentity(a)
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader(checkinReqBody))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。成功与否看业务 code：0 → CheckinDone；9095 → CheckinAlready。
func (c *Client) CheckinClaim(a *auth.Auth) (CheckinResult, error) {
	c.ensureIdentity(a)
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader(checkinReqBody))
	if err != nil {
		return CheckinDisabled, err
	}
	UgHeaders(req, a)
	if _, err := c.doJSON(req); err != nil {
		// 9095 = 今日已签到（幂等成功，不是失败）。
		var ue *Error
		if errors.As(err, &ue) && ue.Code == checkinAlreadyCode {
			return CheckinAlready, nil
		}
		return CheckinDisabled, err
	}
	return CheckinDone, nil
}

// entUsageBody ide_user_ent_usage 的请求体。真实客户端发这两个字段；
// 发空对象 `{}` 拿到的 usage 不完整（2026-09-03 抓包实测）。
var entUsageBody = []byte(`{"require_usage":true,"req_source":2}`)

// UserEntUsage 聚合积分（仅未过期权益包的剩余额度）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	remain, _, _, _, err = c.EntUsage(a)
	return remain, err
}

// EntUsage 查询账号额度明细（权益包总量/已用/剩余/包数）。
// remain = limit - used，usage.credits_amount 是已用积分（实测）。
//
// **过期包必须跳过**：签到积分是「当日发放、31 天后过期」的独立包。不过滤就是把
// 历史上所有签到包的额度都算进「剩余」，面板越签越多、永远用不完，且掩盖真实余额。
func (c *Client) EntUsage(a *auth.Auth) (remain, limit, used int64, packs int, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader(entUsageBody))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
				EndTime int64 `json:"end_time"`
			} `json:"entitlement_base_info"`
			ExpireTime int64 `json:"expire_time"`
			Usage      struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	nowSec := time.Now().Unix()
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		// 过期判定：end_time / expire_time 为 Unix 秒；缺失或 0 视作不过期。
		et := p.EntitlementBaseInfo.EndTime
		if et == 0 {
			et = p.ExpireTime
		}
		if et > 0 && et <= nowSec {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs++
	}
	return remain, limit, used, packs, nil
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
