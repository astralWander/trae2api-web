package upstream

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":1005,"message":"plan limit","extra":{"plan":2}}`, ErrPlanLimit},
		{200, `{"code":1005,"msg":"权益不足"}`, ErrPlanLimit},
		{429, ``, ErrSoftRate},
		{401, `{"code":1001,"msg":"login required"}`, ErrSessionDead},
		{401, ``, ErrSessionDead},
		{404, ``, ErrNotFound},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		{200, `{"checked_in":false}`, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:      &http.Client{Transport: fn},
		AgentHost: "https://agent.example",
		UgHost:    "https://ug.example",
		OAuthHost: "https://oauth.example",
		ClientID:  ClientID,
	}
}

func TestRefreshTokenExchange(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpExchange) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			return nil, errors.New("missing content-type")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ClientID":"en1oxy7wnw8j9n"`)) || !bytes.Contains(body, []byte(`"RefreshToken":"oldrt"`)) {
			return nil, errors.New("bad body: " + string(body))
		}
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt != 1786805537 {
		t.Errorf("expiresAt=%d", a.ExpiresAt)
	}
}

// TestRefreshTokenExchangeMilliseconds 覆盖上游 TokenExpireAt 返回毫秒的场景：
// 必须归一化为 Unix 秒后再写 auth.ExpiresAt。
func TestRefreshTokenExchangeMilliseconds(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1786847930 {
		t.Errorf("expiresAt=%d want 1786847930 (毫秒转秒)", a.ExpiresAt)
	}
}

func TestRefreshTokenIfNeededSkipsFresh(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("fresh token should not refresh")
	}
	if calls != 0 {
		t.Errorf("ExchangeToken should not be called, calls=%d", calls)
	}
	if a.AccessToken != "at" {
		t.Error("token should remain unchanged")
	}
}

func TestRefreshTokenIfNeededRefreshesExpired(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || calls != 1 {
		t.Errorf("expired token should refresh once, refreshed=%v calls=%d", refreshed, calls)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

func TestRefreshTokenUsesAuthApiHost(t *testing.T) {
	var gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Scheme + "://" + r.URL.Host
		return jsonResp(200, `{"Result":{"Token":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://custom.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if gotHost != "https://custom.example" {
		t.Errorf("host=%s want auth.apiHost", gotHost)
	}
}

func TestChatStreamSendsHeadersAndRewritesBody(t *testing.T) {
	var gotAuth, gotUID, gotAppID, gotIdeVer string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-Uid")
		gotAppID = r.Header.Get("X-App-Id")
		gotIdeVer = r.Header.Get("X-Ide-Version")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Cloud-IDE-JWT at" || gotUID != "u1" {
		t.Errorf("headers: auth=%q uid=%q", gotAuth, gotUID)
	}
	if gotAppID != AppID || gotIdeVer != IdeVersion {
		t.Errorf("app headers: appid=%q idever=%q", gotAppID, gotIdeVer)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) || !bytes.Contains(gotBody, []byte(`"function":"solo_work_lite"`)) {
		t.Errorf("body not rewritten: %s", gotBody)
	}
}

func TestChatStreamUsesDedicatedStreamClient(t *testing.T) {
	// StreamHTTP 优先于 HTTP 被 ChatStream 使用（无总超时的长 SSE 流客户端）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	c.StreamHTTP = &http.Client{Transport: c.HTTP.Transport} // 无 Timeout
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("stream client should have no total timeout, got %v", c.StreamHTTP.Timeout)
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `rate limited`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 429 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("429 should come via status, err=%v", err)
	}
	if Classify(status, string(respBody)) != ErrSoftRate {
		t.Errorf("not classified soft rate: %q", respBody)
	}
}

func TestUserEntUsageAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" {
			return nil, errors.New("missing auth header")
		}
		return jsonResp(200, `{"is_credits_billing":true,"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":2000}}},
			{"entitlement_base_info":{"quota":{"credits_limit":500}}}
		]}`), nil
	})
	remain, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("ent usage: %v", err)
	}
	if remain != 2500 {
		t.Errorf("remain=%d want 2500", remain)
	}
}

func TestCheckinStatusAndClaim(t *testing.T) {
	var path string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		if r.Header.Get("X-User-Region") != "CN" {
			return nil, errors.New("missing X-User-Region")
		}
		return jsonResp(200, `{"checked_in":false,"credits":200,"enable":true}`), nil
	})
	checkedIn, credits, enable, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if checkedIn || !enable || credits != 200 {
		t.Errorf("status: checked=%v enable=%v credits=%d", checkedIn, enable, credits)
	}
	if path != EpCheckinStatus {
		t.Errorf("path=%s", path)
	}
}

func TestClassifyBusiness(t *testing.T) {
	cases := map[int64]ErrKind{
		0:    ErrNone,
		1001: ErrSessionDead,
		1005: ErrPlanLimit,
		4008: ErrPlanLimit,
		4011: ErrSoftRate,
		9074: ErrCheckinDenied,
		9999: ErrClient,
	}
	for code, want := range cases {
		if got := ClassifyBusiness(code); got != want {
			t.Errorf("ClassifyBusiness(%d)=%v want %v", code, got, want)
		}
	}
}

// HTTP 200 + 业务错误码必须被识别为错误，而不是被当成"未开放"或"成功"——
// 这是签到失败却报 checkin disabled / 假成功的根因。
func TestCheckinStatusBusinessErrorSurfaced(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":9074,"message":"当前使用人数太多"}`), nil
	})
	_, _, _, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("HTTP200 business code 9074 must surface as error")
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrCheckinDenied || ue.Code != 9074 {
		t.Fatalf("err=%v", err)
	}
}

// claim 返回 200+9074 时，Checkin 绝不能报成功。
func TestCheckinClaimBusinessErrorNotSilent(t *testing.T) {
	old := checkinRetryDelays
	checkinRetryDelays = nil
	defer func() { checkinRetryDelays = old }()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return jsonResp(200, `{"code":9074,"message":"当前使用人数太多"}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":200,"enable":true}`), nil
	})
	if _, err := c.Checkin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("claim business error must not be reported as success")
	}
}

// 9074 应做一次快速重试，重试后成功返回 CheckinDone（并回查 status 确认）。
func TestCheckinRetriesDeniedThenSucceeds(t *testing.T) {
	old := checkinRetryDelays
	checkinRetryDelays = []time.Duration{time.Millisecond}
	defer func() { checkinRetryDelays = old }()

	var claimN int
	done := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			claimN++
			if claimN < 2 {
				return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
			}
			done = true
			return jsonResp(200, `{"code":0,"message":"success"}`), nil
		}
		if done { // claim 后回查：上游应标记 checked_in
			return jsonResp(200, `{"checked_in":true,"credits":100,"enable":true}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":100,"enable":true}`), nil
	})
	res, err := c.Checkin(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != CheckinDone || claimN != 2 {
		t.Fatalf("res=%v claimN=%d want CheckinDone/2", res, claimN)
	}
}

// 9074 连续多次后成功：应完整走完退避重试序列（覆盖 1+len(delays) 次尝试）。
func TestCheckinRetriesDeniedMultipleTimesThenSucceeds(t *testing.T) {
	old := checkinRetryDelays
	checkinRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { checkinRetryDelays = old }()

	const failN = 4 // 前 4 次 9074，第 5 次成功
	var claimN int
	done := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			claimN++
			if claimN <= failN {
				return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
			}
			done = true
			return jsonResp(200, `{"code":0,"message":"success"}`), nil
		}
		if done {
			return jsonResp(200, `{"checked_in":true,"credits":100,"enable":true}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":100,"enable":true}`), nil
	})
	res, err := c.Checkin(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != CheckinDone || claimN != failN+1 {
		t.Fatalf("res=%v claimN=%d want CheckinDone/%d", res, claimN, failN+1)
	}
}

// 9074 重试全部耗尽仍失败 → 返回错误且带 code 9074（绝不谎报成功）。
//
// 同时锁定「换设备号」策略：同设备只重试 1 次（本用例 delays 收成 1 个），
// 之后每次重试都必须换一个**全新**的设备号。
func TestCheckinRetriesExhaustedFails(t *testing.T) {
	oldDelays, oldMax, oldDelay := checkinRetryDelays, checkinMaxDeviceRotations, checkinRotateDelay
	checkinRetryDelays = []time.Duration{time.Millisecond}
	checkinMaxDeviceRotations = 2
	checkinRotateDelay = time.Millisecond
	defer func() {
		checkinRetryDelays, checkinMaxDeviceRotations, checkinRotateDelay = oldDelays, oldMax, oldDelay
	}()

	var devs []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			devs = append(devs, r.Header.Get("X-Device-Id"))
			return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":100,"enable":true}`), nil
	})
	_, err := c.Checkin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("exhausted retries must fail")
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Code != 9074 {
		t.Fatalf("err=%v want code 9074", err)
	}
	want := 1 + len(checkinRetryDelays) + checkinMaxDeviceRotations // 首试 + 同设备重试 + 换号重试
	if len(devs) != want {
		t.Fatalf("claim 次数=%d want %d（%v）", len(devs), want, devs)
	}
	if devs[0] == "" || devs[0] != devs[1] {
		t.Fatalf("前两次应沿用同一设备号: %v", devs)
	}
	seen := map[string]bool{devs[0]: true}
	for _, d := range devs[2:] {
		if !auth.IsRealDeviceID(d) {
			t.Fatalf("换号后应是 15~16 位纯数字，得到 %q（%v）", d, devs)
		}
		if seen[d] {
			t.Fatalf("设备号被重复使用：%q（%v）", d, devs)
		}
		seen[d] = true
	}
	if !strings.Contains(err.Error(), "已轮换 2 个 deviceId") {
		t.Fatalf("错误应说明已换号次数，实际: %v", err)
	}
}

// 9074 之后换新设备号 → 签到成功（这就是 3 个账号实测的路径）。
func TestCheckinRotatesDeviceOn9074ThenSucceeds(t *testing.T) {
	oldDelays, oldMax, oldDelay := checkinRetryDelays, checkinMaxDeviceRotations, checkinRotateDelay
	checkinRetryDelays = []time.Duration{time.Millisecond} // 同设备只重试 1 次
	checkinMaxDeviceRotations = 3
	checkinRotateDelay = time.Millisecond
	defer func() {
		checkinRetryDelays, checkinMaxDeviceRotations, checkinRotateDelay = oldDelays, oldMax, oldDelay
	}()

	var devs []string
	done := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			devs = append(devs, r.Header.Get("X-Device-Id"))
			if len(devs) < 3 { // 原号两次 9074，换号后成功
				return jsonResp(200, `{"code":9074,"message":"当前参与用户太多"}`), nil
			}
			done = true
			return jsonResp(200, `{"code":0,"message":"success"}`), nil
		}
		if done {
			return jsonResp(200, `{"checked_in":true,"credits":100,"enable":true}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":100,"enable":true}`), nil
	})
	a := &auth.Auth{AccessToken: "at"}
	res, err := c.Checkin(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != CheckinDone || len(devs) != 3 {
		t.Fatalf("res=%v claimN=%d want CheckinDone/3", res, len(devs))
	}
	if devs[0] != devs[1] {
		t.Fatalf("前两次应同号: %v", devs)
	}
	if devs[2] == devs[0] || !auth.IsRealDeviceID(devs[2]) {
		t.Fatalf("第三次应换成新的 16 位数字号: %v", devs)
	}
	if got := a.DeviceIDValue(); got != devs[2] {
		t.Fatalf("Auth 上的 deviceId 应已更新为 %q，实际 %q", devs[2], got)
	}
}

// claim 返回 code 0 但回查 status 仍 checked_in=false → 判失败，绝不谎报成功。
func TestCheckinCodeZeroButNotEffective(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return jsonResp(200, `{"code":0,"message":"success"}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":100,"enable":true}`), nil
	})
	if _, err := c.Checkin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("code 0 without checked_in must be treated as failure")
	}
}

// claim 返回 9095（今日已签到）→ 幂等成功，报 already。
func TestCheckinClaim9095IsAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return jsonResp(200, `{"code":9095,"message":"今日已签到"}`), nil
		}
		return jsonResp(200, `{"checked_in":false,"credits":100,"enable":true}`), nil
	})
	res, err := c.Checkin(&auth.Auth{AccessToken: "at"})
	if err != nil || res != CheckinAlready {
		t.Fatalf("res=%v err=%v want CheckinAlready", res, err)
	}
}

// claim/status 请求体为空对象，且带上 ug 插件头与 X-Device-Id。
func TestCheckinRequestShape(t *testing.T) {
	var gotBody []byte
	var gotUA, gotAccept, gotDev, gotMkt string
	a := &auth.Auth{AccessToken: "at"}
	_ = a.EnsureDeviceID()
	_ = a.EnsureMarketUserID()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotDev = r.Header.Get("X-Device-Id")
		gotMkt = r.Header.Get("X-Market-User-Id")
		return jsonResp(200, `{"checked_in":true,"credits":100,"enable":true}`), nil
	})
	if _, _, _, err := c.CheckinStatus(a); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != "{}" {
		t.Errorf("body=%q want {}", string(gotBody))
	}
	if gotUA != UgUserAgent {
		t.Errorf("UA=%q want %q", gotUA, UgUserAgent)
	}
	if gotAccept != "*/*" {
		t.Errorf("Accept=%q want */*", gotAccept)
	}
	if !auth.IsRealDeviceID(gotDev) {
		t.Errorf("X-Device-Id=%q not a real 16-digit device id", gotDev)
	}
	if gotMkt == "" {
		t.Errorf("X-Market-User-Id missing")
	}
}

// 过期权益包必须跳过，否则历史签到包会把「剩余」越算越多。
func TestEntUsageSkipsExpiredPacks(t *testing.T) {
	now := time.Now().Unix()
	expired := now - 3600
	future := now + 3600
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":1000},"end_time":`+itoa(future)+`},"usage":{"credits_amount":200}},
			{"entitlement_base_info":{"quota":{"credits_limit":500},"end_time":`+itoa(expired)+`},"usage":{"credits_amount":0}}
		]}`), nil
	})
	remain, limit, used, packs, err := c.EntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if remain != 800 || limit != 1000 || used != 200 || packs != 1 {
		t.Fatalf("remain=%d limit=%d used=%d packs=%d want 800/1000/200/1", remain, limit, used, packs)
	}
}

func TestCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"checked_in":true,"credits":0,"enable":true}`), nil
	})
	res, err := c.Checkin(&auth.Auth{AccessToken: "at"})
	if err != nil || res != CheckinAlready {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

func TestCheckinDisabled(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"checked_in":false,"credits":0,"enable":false}`), nil
	})
	res, err := c.Checkin(&auth.Auth{AccessToken: "at"})
	if err != nil || res != CheckinDisabled {
		t.Fatalf("res=%v err=%v", res, err)
	}
}
