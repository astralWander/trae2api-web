package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Pair
	}{
		{"basic", "A=1", []Pair{{"A", "1"}}},
		{"spaces", "  A  =  1  ", []Pair{{"A", "1"}}},
		{"empty value", "A=", []Pair{{"A", ""}}},
		{"empty value with space", "A=   ", []Pair{{"A", ""}}},
		{"comment line", "# note\nA=1", []Pair{{"A", "1"}}},
		{"blank lines", "\n\nA=1\n\n", []Pair{{"A", "1"}}},
		{"export prefix", "export A=1", []Pair{{"A", "1"}}},
		{"export tab", "export\tA=1", []Pair{{"A", "1"}}},
		{"not export prefix", "exportFOO=1", []Pair{{"exportFOO", "1"}}},
		{"double quotes", `A="x y"`, []Pair{{"A", "x y"}}},
		{"single quotes", `A='x y'`, []Pair{{"A", "x y"}}},
		{"double quote escapes", `A="a\nb\tc\"d\\e"`, []Pair{{"A", "a\nb\tc\"d\\e"}}},
		{"single quote no escape", `A='a\nb'`, []Pair{{"A", `a\nb`}}},
		{"hash inside value", "A=ab#cd", []Pair{{"A", "ab#cd"}}},
		{"hash comment after space", "A=ab # comment", []Pair{{"A", "ab"}}},
		{"hash inside quotes", `A="x # y"`, []Pair{{"A", "x # y"}}},
		{"CRLF", "A=1\r\nB=2\r\n", []Pair{{"A", "1"}, {"B", "2"}}},
		{"underscore key", "_A_1=x", []Pair{{"_A_1", "x"}}},
		{"value with equals", "A=b=c", []Pair{{"A", "b=c"}}},
		{"unicode value", "A=中文值", []Pair{{"A", "中文值"}}},
		{"comment with unicode", "# API Key（Bearer 鉴权）。\nTW2A_API_KEY=123456", []Pair{{"TW2A_API_KEY", "123456"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, invalid := Parse([]byte(c.in))
			if len(invalid) != 0 {
				t.Fatalf("unexpected invalid lines: %v", invalid)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d pairs %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("pair %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	in := "A=1\nnoequals\n1BAD=2\n=3\nB=2\n"
	got, invalid := Parse([]byte(in))
	if len(got) != 2 {
		t.Fatalf("valid pairs = %v, want 2", got)
	}
	if len(invalid) != 3 {
		t.Fatalf("invalid = %v, want 3 entries", invalid)
	}
	// 行号必须准确，方便用户按提示去改
	for _, want := range []string{"2: noequals", "3: 1BAD=2", "4: =3"} {
		found := false
		for _, s := range invalid {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %v", want, invalid)
		}
	}
}

// TestParseInvalidTruncatesLongLine 防止坏行把日志刷爆。
func TestParseInvalidTruncatesLongLine(t *testing.T) {
	long := strings.Repeat("x", 200)
	_, invalid := Parse([]byte(long))
	if len(invalid) != 1 {
		t.Fatalf("invalid = %v", invalid)
	}
	if len(invalid[0]) > 60 {
		t.Errorf("invalid line not truncated: len=%d", len(invalid[0]))
	}
	if !strings.HasSuffix(invalid[0], "...") {
		t.Errorf("truncated line should end with ...: %q", invalid[0])
	}
}

// isolateEnv 保证测试结束后恢复指定变量的原值。
func isolateEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.env")
	res, err := Load(missing)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(res.Loaded) != 0 || len(res.Kept) != 0 || len(res.Invalid) != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
	if res.Path != missing {
		t.Errorf("Path = %q, want %q", res.Path, missing)
	}
}

func TestLoadInjectsEnv(t *testing.T) {
	const k = "TW2A_DOTENV_TEST_A"
	isolateEnv(t, k)
	_ = os.Unsetenv(k)

	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte("# c\n"+k+"=hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(k); got != "hello world" {
		t.Errorf("env = %q, want %q", got, "hello world")
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != k {
		t.Errorf("Loaded = %v, want [%s]", res.Loaded, k)
	}
}

// TestLoadDoesNotOverrideExistingEnv 是核心纪律：env 优先于 .env。
func TestLoadDoesNotOverrideExistingEnv(t *testing.T) {
	const k = "TW2A_DOTENV_TEST_B"
	t.Setenv(k, "from-shell")

	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte(k+"=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(k); got != "from-shell" {
		t.Errorf("env was overwritten: got %q, want from-shell", got)
	}
	if len(res.Kept) != 1 || res.Kept[0] != k {
		t.Errorf("Kept = %v, want [%s]", res.Kept, k)
	}
	if len(res.Loaded) != 0 {
		t.Errorf("Loaded = %v, want empty", res.Loaded)
	}
}

// TestLoadTreatsEmptyEnvAsExisting 明确语义：显式设为空串的环境变量也算「已存在」。
func TestLoadTreatsEmptyEnvAsExisting(t *testing.T) {
	const k = "TW2A_DOTENV_TEST_C"
	if err := os.Setenv(k, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(k) })

	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte(k+"=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(f); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(k); got != "" {
		t.Errorf("env = %q, want empty (should not be filled from file)", got)
	}
}

func TestLoadReportsInvalidLines(t *testing.T) {
	const k = "TW2A_DOTENV_TEST_D"
	isolateEnv(t, k)
	_ = os.Unsetenv(k)

	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte("garbage line\n"+k+"=ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invalid) != 1 || res.Invalid[0] != "1: garbage line" {
		t.Errorf("Invalid = %v", res.Invalid)
	}
	// 坏行不影响好行生效
	if got := os.Getenv(k); got != "ok" {
		t.Errorf("env = %q, want ok", got)
	}
}

// TestLoadRealWorldEnvExample 用仓库里那份 .env.example 的真实内容跑一遍。
func TestLoadRealWorldEnvExample(t *testing.T) {
	pairs, invalid := Parse([]byte("# API Key（Bearer 鉴权）。changeme 是占位符，请换成你自己的随机密钥。\nTW2A_API_KEY=changeme\n"))
	if len(invalid) != 0 {
		t.Fatalf("invalid = %v", invalid)
	}
	if len(pairs) != 1 || pairs[0].Key != "TW2A_API_KEY" || pairs[0].Value != "changeme" {
		t.Fatalf("got %v", pairs)
	}
}
