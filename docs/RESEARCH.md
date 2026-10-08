# TRAE 反代研究笔记

> 本仓库的记录：TRAE（字节跳动 AI IDE）各通道的反代可行性、积分体系、限流机制与实测发现。
> 记录时间：2026-08。部分结论来自公开逆向调查（文末标注来源），部分为本地实测。

---

## 1. 上游反代项目全景

| 项目 | 通道 | 说明 |
|---|---|---|
| [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) | `solo_work_lite` | Go，本仓库上游。SOLO 免费对话 → OpenAI 兼容 |
| [Ttungx/trae-solo-local-api](https://github.com/Ttungx/trae-solo-local-api) | `solo_work_lite` | JS，同通道 |
| [laojichao/trae-local-api](https://github.com/laojichao/trae-local-api) | Trae IDE (CN/SOLO/SG) | Node，四版本，tc 加密 auth 自动解密 |
| [muskke/trae-api-proxy](https://github.com/muskke/trae-api-proxy) | Trae API | Go，Header 签名 + payload 转换 |
| [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | 腾讯 WorkBuddy | **注意：WorkBuddy ≠ TRAE Work**，不同产品 |

所有可用的反代都走 **SOLO/IDE 免费通道（消耗 ide_credits）**，没有任何项目反代 work 通道（work_credits）。

## 2. SOLO 通道机制

- 对话端点：`POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat`
- 关键参数：`function: "solo_work_lite"`（实测：其他 function 值如 `work`/`solo`/`work_lite` 均无效）
- 模型表：`POST /api/ide/v1/get_detail_param`（`config_name` 列表，动态下发）
- 鉴权：`Cloud-IDE-JWT <accessToken>` + `X-Cloudide-Token` + `X-Uid` + `X-Machine-Id` / `X-Device-Id`
- token 获取：`refreshToken` → `ExchangeToken`（轮换 refresh）→ `GetUserInfo`（uid）
- 登录：`https://www.trae.cn/authorization` 强制回调 `127.0.0.1`（浏览器与服务器无需同机，回调链接粘贴即可）
- SSE 格式：`event:notify_usage` / `event:metadata` / `event:error` / `event:done`

### 版本号与模型解锁（实测）

- 上游按 `X-Ide-Version` / `X-App-Version-Code` 版本控制模型可用性
- 实测：`0.1.43` 请求 `glm-5.3` 报 `4001 param is invalid`；**`0.1.52`（20260811）正常对话**
- 模型列表随版本动态更新（`get_detail_param` 0.1.52 返回 35 个 config，含 glm-5.3）

## 3. 积分体系（实测）

对话流中 `event:notify_usage` 返回计费结构：

```json
"billing_mode": "credits",
"cn_credits_remain_info": {"ide_credits": 0, "work_credits": 2000}
```

| 类型 | 用途 | 反代可用 |
|---|---|---|
| `ide_credits` | SOLO 对话（`solo_work_lite`） | 可用（本项目采用） |
| `work_credits` | TRAE Work 编程 Agent | 不可用（见下文说明） |

- 额度查询：`POST /trae/api/v2/pay/ide_user_ent_usage`（聚合 `user_entitlement_pack_list[].entitlement_base_info.quota.credits_limit`，`usage.credits_amount` 为已用）
  - body 必须为 `{"require_usage":true,"req_source":2}`（发 `{}` 拿不到完整 usage）
  - **必须跳过已过期权益包**（`end_time`/`expire_time` ≤ now）：签到积分是「当日发放、31 天后过期」的独立包，不过滤会把历史签到包累加进「剩余」，面板越签越多
- **注意**：`ide_user_ent_usage` 聚合的是 entitlement 包（含 work 包），显示 `remain=2000` 实为 work_credits，**不代表 SOLO 通道可用额度**。SOLO 真正看 `notify_usage.cn_credits_remain_info.ide_credits`
- 签到：`POST /trae/api/v2/ug/checkin_credits/status` + `/claim`，body 均为 `{}`（2026-09-03 抓包实测）
  - **ug 链路伪装的是 VSCode 插件进程**，与 chat 链路不是同一套身份：UA 必须是 `VSCode 1.107.1 (TRAE SOLO CN)`（不是 `Trae/0.1.61`），且带 `Accept: */*`、`Package-Type: stable_cn`、`X-Market-Client-Id`、`X-Request-Id`、`X-TT-Trace-Id`、`Sec-Fetch-*` 等（本项目 `UgHeaders` 已逐头对齐）
  - **`X-Device-Id` 必须是 15~16 位纯数字**（真实客户端实测如 `1711320556112436`）：发 hex32 / UUID 在风控眼里不是设备号 → **稳定 9074**；缺失 → `code 9004`。本项目会自动把存量 hex32 迁移为数字形态（判重按账号，迁移不会重复签到）
  - `9074 当前参与用户太多` 的成因（2026-10-08 实测定性，**前两版结论都被推翻**）：**不是**「瞬时限流」（版本一），也**不是**「账号级稳定拒绝 / 重试必空」（版本二，参考 dsh-router-traework）。真正被拒的是 **(账号, deviceId) 组合**——同一账号同一时刻对照：原设备号连续 20 次 claim 全 9074（间隔 3s、约 1 分钟），换一个**全新 16 位数字 deviceId** 立即 `code 0`；3 个失败账号全部这样签上，积分各 +100。被拒过的设备号会**持续**被拒（非数秒恢复），换号才有机会，且新号也并非一次必中（实测 4 次新号：3 次一把成功、1 次第二把成功）。本项目策略：**同设备快重试 1 次（1s + 抖动）→ 仍 9074 就换新设备号，最多 4 个**；换号后必须 `SaveAtomic` 落盘，否则重启又回到旧号
  - 业务码：`0` 成功（已签到后重复调用也返回 0，幂等）、`9095` 今日已签到、`9074` 签到拒绝（换设备号重试）、`1001` 会话失效
  - 判定规则：只看 HTTP 状态会误报成功（一律 200）；claim 拿到 code 0 也要**回查 `status.checked_in`** 才算数；**积分不能当签到凭据**（是所有包的聚合剩余额度）
  - 单日奖励 `status.credits + extra_credits`（免费 100，会员更高）；已签到判定用 `checked_in`
- ide_credits 耗尽：对话报 `4008 Your requests have exceeded the quota`（视为配额限制，短时重试无效，等每日重置或签到）

### 上下文缓存（prompt cache，2026-10-08 实测）

上游**确实有前缀缓存**，命中时在 `event:token_usage` 里以 **Anthropic 口径**返回：

```json
{"prompt_tokens":2033, "cache_read_input_tokens":1792, "cache_creation_input_tokens":0,
 "completion_tokens":137, "total_tokens":2170, "reasoning_tokens":135, "cluster":"normal_context"}
```

- **与「会话粘性」无关，也与账号无关**：用同一段 2033 token 的固定前缀对照 —— ① 不带 `session_id`（上游每次自分配新 session）；② 三次 `session_id` 互不相同；③ **换到另一个账号**（该账号首次访问此前缀）—— `cache_read_input_tokens` 均为 1792（回访时 1920）。⇒ 缓存键是**前缀内容本身**，落在上游的共享缓存池上（metadata 里的 `prompt_cache_pool_record` 即指向它），不是 per-session / per-account。
- 实测 `prompt_cache_pool_record` 在无 session 复用时为 `null`，但缓存照样命中 —— 可佐证缓存不依赖会话。
- **命中只影响计费**：`prompt_tokens` 仍按全量计（2033，其中 1792 命中）；`cache_creation_input_tokens` 在观察期内恒为 0（未见按写入计费）。
- **坑（本仓库已修）**：这组字段名是 Anthropic 口径，原样透传时，读 OpenAI 规范（`usage.prompt_tokens_details.cached_tokens`）或 DeepSeek 口径（`usage.prompt_cache_hit_tokens`）的客户端**一个都读不到** → 界面上恒显示「缓存 0」，看起来像完全没命中。`normalizeUsage` 现在**只做加法**：上游原字段保留，另补 `prompt_tokens_details.cached_tokens`、`prompt_cache_hit_tokens`/`prompt_cache_miss_tokens`、`completion_tokens_details.reasoning_tokens`。
- 本项目**不透传也不补发 `session_id`**（客户端请求体原样透传，上游自分配）；实测这与缓存命中无关。

## 4. work 通道为何不可反代

引用公开逆向调查（[rosemarycox5334-debug/PA_Agent → TRADE_WORK_CN_INVESTIGATION.md](https://github.com/rosemarycox5334-debug/PA_Agent/blob/main/TRADE_WORK_CN_INVESTIGATION.md)，2026-08，**未在本仓库复现，引用结论**）：

| 路径 | 结果 |
|---|---|
| `llm_raw_chat` v1/v2 | 外部调用严格限流（4011）；v2 仅服务器内部从 `create_agent_task` 调用不限流 |
| `create_agent_task`（work agent 编排） | 需要 `encrypted_prompt_set` 加密字段——只存在于本地 ai-agent 进程加密 DB，外部无法构造 |
| IPC（`lite/send_message`） | 需要完整客户端注册流程 + 绑定 session_id |

**结论：work 通道钥匙（encrypted_prompt_set）烧在本地 ai-agent 进程内，反代不可行**。TRAE Work 客户端本身是"本地 agent 构造加密任务 → 云端执行"架构。

## 5. 限流 / 错误码速查（实测 + 上游代码）

| code | 含义 | 处理 |
|---|---|---|
| 1001 | 认证失败（token 失效） | 重新登录（换 refreshToken） |
| 1005 | plan 权益不足 | 长冷却（12h） |
| 4001 | 参数无效（模型不存在/版本不匹配） | 升级 `IdeVersion` 或换模型 |
| 4008 | 配额超限（ide_credits 耗尽） | 等每日重置 / 签到 |
| 4011 | 请求频率超限 | 等限流窗口 |
| 9004 | 签到参数错误（缺 `X-Device-Id`） | 补设备头（须 15~16 位数字形态） |
| 9074 | 签到被拒（(账号, deviceId) 组合被持续拒绝，非 IP / 非账号级） | 同设备重试 1 次 → 换新设备号重试（最多 4 个，已内置） |
| 429 | 软限流 | 短冷却（60s） |

## 6. Windows 环境坑（本仓库实测修复）

- **python3 商店占位别名**：`C:\Users\...\WindowsApps\python3` 指向 `AppInstallerPythonRedirector.exe`，非交互下恒 exit 49（连 `print('hi')` 都失败）。修复：检测后回退 `python`
- **GBK 写文件炸**：`open("w")` 默认 GBK，遇非 GBK 字符抛 `UnicodeEncodeError` 且**先清空已有文件**（实测损坏凭证）。修复：UTF-8 + tmp/rename 原子写
- **回调昵称双重编码**：TRAE 回调 `userInfo` 中文双重 URL 编码，`parse_qs` 一层解不干净 → 昵称乱码（实测 `Óû§8847309959`）。修复：`fix_mojibake` 回转失败则回退 `用户+uid末4位`

## 7. 结论

1. **SOLO 免费通道（ide_credits）是唯一可反代的 TRAE 通道**，上游刻意放行 `solo_work_lite`（免费引流，额度天花板 + 限流兜底）
2. **work_credits（TRAE Work）无法通过 API 反代**——加密配置锁在本地进程，商业上也不可能开放
3. 反代的价值场景：作为 **Claude Code / Cline / Codex 等自带 agent 编排客户端的模型后端**（OpenAI 兼容 + function calling），而非替代 work 的云端 agent
