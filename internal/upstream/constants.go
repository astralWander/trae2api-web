// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	Function       = "solo_work_lite"

	// 签到/积分（api.trae.cn）走的是 **VSCode 插件进程**，UA 与 IDE 主进程的
	// `Trae/{IdeVersion}`(clientUA) 不同（2026-09-03 抓包实测）。三者混用 =
	// 同一账号出现三套客户端身份，风控画像对不上。
	UgUserAgent   = "VSCode 1.107.1 (TRAE SOLO CN)"
	UgAppVersion  = "0.1.61"      // 插件链路的 App-Version（抓包实测值）
	MarketClientID = "VSCode 1.107.1" // X-Market-Client-Id 与 UA 同源但不带 CN 后缀

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)
