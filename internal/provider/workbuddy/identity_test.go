package workbuddy

import (
	"net/http"
	"strings"
	"testing"
)

// 本文件守住"伪装版本可覆盖"这条应急通道。
//
// 🔴 背景（2026-10-05 评审提出，我随后做了证伪实验）：
//
//	`clientUA` / `ideVersion` 是上游客户端的伪装标识，硬编码在源码里。
//	评审担心："若腾讯升级 CLI 版本、旧版本号被拒，项目会突然全部失败。"
//
//	**实测证伪了"会被拒"**（见 docs/维护备忘.md）：
//	把 UA 改成 `CLI/0.0.1`、`99.99.99`、甚至 `curl/8.0`，
//	把 X-IDE-Version 改成 `0.0.1` / `99.99.99` / `not-a-version`，
//	**全部返回 HTTP 200**，模型目录也正常（31 个模型）。
//	→ 上游当前把它们当"用量归属标识"，不是鉴权门槛。
//
//	但"现在不校验"不等于"将来不校验"。所以保留环境变量覆盖通道：
//	真出问题时**改环境变量即可，不必重新编译**（用户手上只有 exe）。

// TestDefaultsWhenNoEnvOverride 守：没设环境变量时用实测默认值。
//
// 默认值必须等于实测捕获值 —— 改了它等于改了伪装身份。
func TestDefaultsWhenNoEnvOverride(t *testing.T) {
	t.Setenv(envClientUA, "")
	t.Setenv(envIDEVersions, "")

	if got := clientUA(); got != clientUADefault {
		t.Errorf("默认 UA = %q，期望 %q", got, clientUADefault)
	}
	if got := ideVersion(); got != ideVersionDefault {
		t.Errorf("默认 IDE 版本 = %q，期望 %q", got, ideVersionDefault)
	}
}

// TestEnvOverrideTakesEffect 守：环境变量能覆盖（应急通道可用）。
func TestEnvOverrideTakesEffect(t *testing.T) {
	t.Setenv(envClientUA, "CLI/9.9.9 CodeBuddy/9.9.9")
	t.Setenv(envIDEVersions, "9.9.9")

	if got := clientUA(); got != "CLI/9.9.9 CodeBuddy/9.9.9" {
		t.Errorf("UA 覆盖未生效，得到 %q", got)
	}
	if got := ideVersion(); got != "9.9.9" {
		t.Errorf("IDE 版本覆盖未生效，得到 %q", got)
	}
}

// TestBlankEnvFallsBackToDefault 守：空串/纯空白**回落到默认**。
//
// 为什么不把空串当"故意清空"：空 UA 在 HTTP 层是异常值，
// 而上游目前不校验 —— 真发出去只会让排查更混乱。
// 想改就用有效值覆盖。
func TestBlankEnvFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"", "   ", "\t"} {
		t.Setenv(envClientUA, v)
		if got := clientUA(); got != clientUADefault {
			t.Errorf("环境变量为 %q 时应回落默认，实际得到 %q", v, got)
		}
	}
}

// TestIdentityVersionsInfoFlagsCustom 守：能区分"默认值"与"被覆盖"。
//
// 排查"是不是版本问题"时，必须先知道"这值是不是有人改过"。
func TestIdentityVersionsInfoFlagsCustom(t *testing.T) {
	t.Setenv(envClientUA, "")
	t.Setenv(envIDEVersions, "")
	info := IdentityVersionsInfo()
	if info.UACustom || info.IDEVersionsCustom {
		t.Error("未设环境变量时不应标记为 custom")
	}
	if info.UA != clientUADefault {
		t.Errorf("UA = %q，期望默认值", info.UA)
	}

	t.Setenv(envClientUA, "CLI/9.9.9 CodeBuddy/9.9.9")
	info2 := IdentityVersionsInfo()
	if !info2.UACustom {
		t.Error("设了环境变量时应标记 UACustom")
	}
	if info2.IDEVersionsCustom {
		t.Error("只覆盖了 UA，IDE 版本不应标记为 custom")
	}
}

// TestSpoofHeadersActuallyUseOverride 守：**真正发出去的请求头**用了覆盖值。
//
// 前面几个测试只验证了取值函数；这条验证调用点真的接上了 ——
// 否则函数返回新值、请求头却仍是旧常量（这类"改了没接上"很难发现）。
func TestSpoofHeadersActuallyUseOverride(t *testing.T) {
	t.Setenv(envClientUA, "CLI/7.7.7 CodeBuddy/7.7.7")
	t.Setenv(envIDEVersions, "7.7.7")

	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyChatHeaders(req, testCred(), "msg-1")

	if got := req.Header.Get("User-Agent"); got != "CLI/7.7.7 CodeBuddy/7.7.7" {
		t.Errorf("请求头 User-Agent = %q，期望覆盖值（说明调用点没接上）", got)
	}
	if got := req.Header.Get("X-IDE-Version"); got != "7.7.7" {
		t.Errorf("请求头 X-IDE-Version = %q，期望覆盖值（说明调用点没接上）", got)
	}
}

// TestSpoofHeadersKeepOtherIdentityFields 守：
// 覆盖版本**不影响**其它伪装字段（别误伤）。
func TestSpoofHeadersKeepOtherIdentityFields(t *testing.T) {
	t.Setenv(envClientUA, "CLI/7.7.7 CodeBuddy/7.7.7")

	req, err := http.NewRequest(http.MethodPost,
		"https://example.invalid/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyChatHeaders(req, testCred(), "msg-1")

	// 这几个是"用量归属"标识，不提供覆盖通道，必须保持实测值
	for k, want := range map[string]string{
		"X-IDE-Name":      ideName,
		"X-IDE-Type":      ideType,
		"X-Agent-Purpose": agentPurpose,
	} {
		if got := req.Header.Get(k); got != want {
			t.Errorf("%s = %q，期望 %q（不应被版本覆盖影响）", k, got, want)
		}
	}
}

// TestNoTokenInUA 守：覆盖值不得成为夹带凭据的通道。
//
// 环境变量是用户可控的；若有人把 token 塞进 UA，会被发到上游。
// 这条不做强制拦截（那可能误伤合法值），但**断言当前实现不会主动拼接凭据**。
func TestNoTokenInUA(t *testing.T) {
	t.Setenv(envClientUA, "")
	ua := clientUA()
	for _, bad := range []string{"Bearer", "accessToken", "refresh"} {
		if strings.Contains(strings.ToLower(ua), strings.ToLower(bad)) {
			t.Errorf("UA 含疑似凭据内容 %q：%s", bad, ua)
		}
	}
}
