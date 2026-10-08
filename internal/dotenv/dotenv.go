// Package dotenv 提供一个极简的 .env 加载器（零依赖）。
//
// 存在的理由：本服务端的配置项只从【进程环境变量】读取（见 cmd/server/config.go
// 的 applyEnv），而 .env 文件只有 docker compose 会自动读取 —— 二进制 / nohup /
// systemd 部署时它形同废纸，于是 TW2A_API_KEY 恒为空，
// GET /admin/api/accounts/export 一直返回 403 api_key_required。
// 本包让二进制自己也能读 .env。
//
// 三条纪律：
//  1. 零依赖：不引入 godotenv，只实现够用的子集；
//  2. env 优先：已存在的环境变量【绝不】被 .env 覆盖，这样 shell export /
//     systemd Environment / docker -e 注入的值永远说了算；
//  3. 坏行不致命：解析失败的行跳过并记录，不影响其余变量生效。
package dotenv

import (
	"os"
	"strconv"
	"strings"
)

// DefaultFile 是默认读取的文件名（相对当前工作目录）。
const DefaultFile = ".env"

// Pair 是一条 KEY=VALUE。
type Pair struct {
	Key   string
	Value string
}

// Result 是一次加载的结果。三个切片里只有变量名和行号，【不含值】，可安全打日志。
type Result struct {
	Path    string   // 实际读取的路径
	Loaded  []string // 本次注入的变量名
	Kept    []string // 因环境变量已存在而跳过（env 优先）的变量名
	Invalid []string // 无法解析的行，形如 "3: ???"
}

// Load 读取 path 并注入进程环境变量。
//
// 文件不存在不算错误（返回零值 Result）；已存在的环境变量不会被覆盖。
func Load(path string) (Result, error) {
	res := Result{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, err
	}

	pairs, invalid := Parse(raw)
	res.Invalid = invalid
	for _, p := range pairs {
		if _, exists := os.LookupEnv(p.Key); exists {
			res.Kept = append(res.Kept, p.Key)
			continue
		}
		if err := os.Setenv(p.Key, p.Value); err != nil {
			return res, err
		}
		res.Loaded = append(res.Loaded, p.Key)
	}
	return res, nil
}

// Parse 解析 .env 文本，返回有效键值对与无法解析的行（含行号）。
//
// 支持：# 注释、空行、可选的 export 前缀、单/双引号包裹、双引号内的 \n \t \r \" \\ 转义、
// 未加引号时的行内注释（# 前须有空白）、CRLF 与 LF 换行。
func Parse(data []byte) (pairs []Pair, invalid []string) {
	lines := strings.Split(string(data), "\n")
	for i, rawLine := range lines {
		lineNo := i + 1
		// 先剥掉 CRLF 里的 \r，再统一去空白（Windows 上编辑的 .env 很常见）
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 可选 export 前缀：仅当 export 后面跟空白时才算
		if rest, ok := trimExportPrefix(line); ok {
			line = rest
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
		}

		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			invalid = append(invalid, formatInvalid(lineNo, line))
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !validKey(key) {
			invalid = append(invalid, formatInvalid(lineNo, line))
			continue
		}
		pairs = append(pairs, Pair{Key: key, Value: parseValue(line[eq+1:])})
	}
	return pairs, invalid
}

// trimExportPrefix 剥掉 "export " / "export\t" 前缀。
func trimExportPrefix(line string) (string, bool) {
	const p = "export"
	if !strings.HasPrefix(line, p) {
		return line, false
	}
	rest := line[len(p):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return line, false // 形如 exportFOO=1，不是前缀
	}
	return strings.TrimSpace(rest), true
}

// parseValue 解析等号右边，去掉引号与行内注释。
func parseValue(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		// 双引号：去引号 + 解转义
		if v[0] == '"' && v[len(v)-1] == '"' {
			return unescape(v[1 : len(v)-1])
		}
		// 单引号：原样取出，不转义
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return v[1 : len(v)-1]
		}
	}
	// 未加引号：只把「空白 + #」当作注释起点。
	// 这样 TW2A_API_KEY=ab#cd 里的 # 会被保留（值里可能真含 #）。
	for i := 0; i < len(v); i++ {
		if v[i] == '#' && i > 0 && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// unescape 处理双引号内的转义序列。
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default: // 未知转义原样保留反斜杠
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// validKey 校验变量名：字母或下划线开头，后接字母/数字/下划线。
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// formatInvalid 生成 "行号: 行内容" 的说明；内容过长时截断，避免刷屏。
func formatInvalid(lineNo int, line string) string {
	const max = 40
	if len(line) > max {
		line = line[:max] + "..."
	}
	return strconv.Itoa(lineNo) + ": " + line
}
