package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// 版本比较（纯函数，更新功能的"要不要更新"判断）
// ═══════════════════════════════════════════════════════════════════

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    []int
		wantDev bool
	}{
		{"0.1.4", []int{0, 1, 4}, false},
		{"v0.1.4", []int{0, 1, 4}, false},
		{"V0.1.4", []int{0, 1, 4}, false},
		{"0.1.4-taoapi", []int{0, 1, 4}, false},
		{"v0.2.0-rc.2", []int{0, 2, 0}, false},
		{"1", []int{1}, false},
		{"1.2", []int{1, 2}, false},
		{"  0.1.5  ", []int{0, 1, 5}, false},
		{"dev", nil, true},
		{"", nil, true},
		{"abc", nil, true},
		{"v", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := ParseVersion(tc.in)
			if got.Dev != tc.wantDev {
				t.Errorf("ParseVersion(%q).Dev = %v，期望 %v", tc.in, got.Dev, tc.wantDev)
			}
			if tc.want == nil {
				return
			}
			if len(got.Numbers) != len(tc.want) {
				t.Fatalf("ParseVersion(%q).Numbers = %v，期望 %v", tc.in, got.Numbers, tc.want)
			}
			for i := range tc.want {
				if got.Numbers[i] != tc.want[i] {
					t.Errorf("ParseVersion(%q).Numbers = %v，期望 %v", tc.in, got.Numbers, tc.want)
					break
				}
			}
		})
	}
}

func TestNeedsUpdate(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
		why         string
	}{
		{"0.1.4", "0.1.5", true, "补丁号更高"},
		{"0.1.4", "0.2.0", true, "次版本更高"},
		{"0.1.4", "1.0.0", true, "主版本更高"},
		{"0.1.4", "0.1.4", false, "相同"},
		{"0.1.5", "0.1.4", false, "本地更新（远端更旧）"},
		{"0.2.0", "0.1.9", false, "远端更旧不该提示"},
		{"0.1.4", "v0.1.5", true, "tag 带 v"},
		{"0.1.4-taoapi", "0.1.5-taoapi", true, "带构建后缀"},
		{"0.2", "0.2.0", false, "缺位按 0 补 ⇒ 相等"},
		{"0.2.0", "0.2", false, "反向缺位也相等"},
		{"0.10.0", "0.9.0", false, "数字比较不是字典序（0.10 > 0.9）"},
		{"0.9.0", "0.10.0", true, "数字比较不是字典序"},

		// 🔴 dev 的两种方向都必须为 false
		{"dev", "0.1.5", false, "开发构建不参与更新（否则开发者每次被提示）"},
		{"", "0.1.5", false, "空版本同样视为开发构建"},
		{"0.1.4", "dev", false, "远端 tag 异常 ⇒ 不敢动"},
		{"0.1.4", "", false, "远端版本空 ⇒ 不敢动"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s→%s", tc.cur, tc.latest), func(t *testing.T) {
			if got := NeedsUpdate(tc.cur, tc.latest); got != tc.want {
				t.Errorf("NeedsUpdate(%q,%q) = %v，期望 %v —— %s",
					tc.cur, tc.latest, got, tc.want, tc.why)
			}
		})
	}
}

func TestNormalizeTag(t *testing.T) {
	cases := map[string]string{
		"v0.1.5":   "0.1.5",
		"V0.1.5":   "0.1.5",
		"0.1.5":    "0.1.5",
		"version1": "version1", // 不是"v+数字"，不该被砍
		"":         "",
	}
	for in, want := range cases {
		if got := NormalizeTag(in); got != want {
			t.Errorf("NormalizeTag(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestParseDigest 守校验值解析 —— 自动更新的安全底线。
func TestParseDigest(t *testing.T) {
	valid := strings.Repeat("a", 64)

	algo, hexs, err := ParseDigest("sha256:" + valid)
	if err != nil {
		t.Fatalf("合法 digest 被拒: %v", err)
	}
	if algo != "sha256" || hexs != valid {
		t.Errorf("解析结果 = (%q,%q)", algo, hexs)
	}

	// 大写 hex 应被接受并归一为小写（GitHub 目前给的是小写，但不必依赖）
	if _, h, err := ParseDigest("SHA256:" + strings.Repeat("A", 64)); err != nil || h != valid {
		t.Errorf("大写 hex 应被接受并归一，err=%v h=%q", err, h)
	}

	// 🔴 关键：缺失/异常 digest 必须**拒绝**，绝不能"宽松跳过"
	bad := []struct {
		in, why string
	}{
		{"", "空 digest ⇒ 拒绝（否则退化成下载即运行）"},
		{"sha256:", "只有算法没有值"},
		{"sha256:abc", "长度不足"},
		{"md5:" + valid, "不支持的算法"},
		{"sha256:" + strings.Repeat("z", 64), "含非 hex 字符"},
		{"abcdef", "缺少算法前缀"},
	}
	for _, tc := range bad {
		if _, _, err := ParseDigest(tc.in); err == nil {
			t.Errorf("ParseDigest(%q) 应当报错 —— %s", tc.in, tc.why)
		}
	}
}

// TestValidateAssetURL 守下载地址白名单。
func TestValidateAssetURL(t *testing.T) {
	ok := []string{
		"https://github.com/tao1677103724/taoapi/releases/download/v0.1.5/taoapi.exe",
		"https://objects.githubusercontent.com/some/path",
		"https://release-assets.githubusercontent.com/x/y",
	}
	for _, u := range ok {
		if err := ValidateAssetURL(u); err != nil {
			t.Errorf("ValidateAssetURL(%q) 应通过，实际: %v", u, err)
		}
	}

	bad := []struct {
		in, why string
	}{
		{"http://github.com/x", "http 明文可被中间人替换"},
		{"https://evil.com/taoapi.exe", "非白名单主机"},
		{"https://github.com.evil.com/x", "后缀伪装（github.com.evil.com）"},
		{"https://notgithub.com/x", "相似名但不是白名单"},
		{"", "空地址"},
		{"ftp://github.com/x", "非 https"},
	}
	for _, tc := range bad {
		if err := ValidateAssetURL(tc.in); err == nil {
			t.Errorf("ValidateAssetURL(%q) 应当拒绝 —— %s", tc.in, tc.why)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════
// 端到端：用假 GitHub 服务器验证检查与下载
// ═══════════════════════════════════════════════════════════════════

// fakeGitHub 起一个假 GitHub API + 假资产服务器，返回可直接用的 Updater。
//
// ⚠️ 两点测试手法说明：
//
//  1. 资产的 URL 必须指向 httptest 的地址（**http**），而生产的
//     ValidateAssetURL 要求 https。所以这里给 Updater 注入
//     `validateURL` 替身放行 http —— 而"拒绝 http"这条本身由
//     TestValidateAssetURL 单独覆盖，两边不互相削弱。
//  2. `digest` 可传空串，用于测"缺失校验值必须拒绝"。
func fakeGitHub(t *testing.T, tag, digest string, asset []byte, opts ...func(*gitHubRelease)) (*Updater, *httptest.Server) {
	t.Helper()

	var srv *httptest.Server
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("请求缺少 User-Agent（GitHub API 要求）")
		}
		rel := &gitHubRelease{TagName: tag}
		rel.Assets = append(rel.Assets, struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}{Name: AssetName, Size: int64(len(asset)), URL: srv.URL + "/asset", Digest: digest})
		for _, o := range opts {
			o(rel)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(asset)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &Updater{
		APIBase:     srv.URL,
		Repo:        "o/r",
		Client:      srv.Client(),
		validateURL: func(string) error { return nil }, // 放行 http（仅测试）
	}, srv
}

// sha256Of 返回 `sha256:<hex>` 形式。
func sha256Of(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func TestLatestReleaseParses(t *testing.T) {
	asset := []byte("hello")
	digest := sha256Of(asset)

	u, _ := fakeGitHub(t, "v0.1.5", digest, asset)
	rel, err := u.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease 失败: %v", err)
	}
	if rel == nil {
		t.Fatal("应返回 release，实际 nil")
	}
	if rel.Tag != "v0.1.5" || rel.Version != "0.1.5" {
		t.Errorf("tag/version = %q/%q", rel.Tag, rel.Version)
	}
	if rel.AssetName != AssetName {
		t.Errorf("asset 名 = %q，期望 %q", rel.AssetName, AssetName)
	}
	if rel.Digest != digest {
		t.Errorf("digest = %q，期望 %q", rel.Digest, digest)
	}
	if rel.Size != int64(len(asset)) {
		t.Errorf("size = %d，期望 %d", rel.Size, len(asset))
	}
}

// TestDownloadAssetVerifiesHash 是自动更新的**安全核心**测试。
func TestDownloadAssetVerifiesHash(t *testing.T) {
	asset := []byte("this is the new taoapi.exe (fake)")
	digest := sha256Of(asset)

	t.Run("校验通过", func(t *testing.T) {
		u, _ := fakeGitHub(t, "v0.1.5", digest, asset)
		rel, err := u.LatestRelease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		res, err := u.DownloadAsset(context.Background(), rel, dir)
		if err != nil {
			t.Fatalf("下载应当成功: %v", err)
		}
		if res.SHA256 != strings.TrimPrefix(digest, "sha256:") {
			t.Errorf("算出的哈希 = %q", res.SHA256)
		}
		// 落盘内容必须与源一致
		got, _ := os.ReadFile(res.Path)
		if string(got) != string(asset) {
			t.Errorf("落盘内容不符")
		}
	})

	t.Run("🔴 哈希不符必须拒绝并删除文件", func(t *testing.T) {
		// digest 故意与内容不匹配（模拟"途中被篡改"）
		u, _ := fakeGitHub(t, "v0.1.5", sha256Of([]byte("different")), asset)
		rel, _ := u.LatestRelease(context.Background())
		dir := t.TempDir()
		if _, err := u.DownloadAsset(context.Background(), rel, dir); err == nil {
			t.Fatal("哈希不符时必须报错 —— 这是自动更新的安全底线")
		}
		// 半成品必须被清理，不能留在磁盘上
		if _, statErr := os.Stat(filepath.Join(dir, AssetName+".new")); statErr == nil {
			t.Error("校验失败后临时文件未删除")
		}
	})

	t.Run("🔴 digest 缺失必须拒绝（不得宽松跳过）", func(t *testing.T) {
		u, _ := fakeGitHub(t, "v0.1.5", "", asset)
		rel, _ := u.LatestRelease(context.Background())
		dir := t.TempDir()
		if _, err := u.DownloadAsset(context.Background(), rel, dir); err == nil {
			t.Fatal("没有校验值时必须拒绝 —— 否则退化成'下载什么就跑什么'")
		}
	})

	t.Run("🔴 下载地址非白名单必须拒绝", func(t *testing.T) {
		// 这里用**生产的**校验器（不注入替身）来验证闸门真的生效
		u := &Updater{APIBase: "https://api.github.com", Repo: "o/r"}
		rel := &Release{
			AssetURL: "https://evil.example.com/taoapi.exe",
			Digest:   sha256Of(asset),
		}
		if _, err := u.DownloadAsset(context.Background(), rel, t.TempDir()); err == nil {
			t.Fatal("非白名单主机必须拒绝")
		}
	})
}

// TestLatestReleaseNoReleases 守：仓库还没有 Release 时是 (nil,nil) 而非错误。
//
// 委托人当前的真实状态就是"零 Release" —— 这个分支必须走得通，
// 面板应显示"暂无更新"而不是报错。
func TestLatestReleaseNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(srv.Close)

	u := &Updater{APIBase: srv.URL, Repo: "o/r", Client: srv.Client()}
	rel, err := u.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("404 不应报错，实际: %v", err)
	}
	if rel != nil {
		t.Errorf("404 应返回 nil，实际 %+v", rel)
	}
}

func TestLatestReleaseHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(srv.Close)

	u := &Updater{APIBase: srv.URL, Repo: "o/r", Client: srv.Client()}
	if _, err := u.LatestRelease(context.Background()); err == nil {
		t.Fatal("500 应当报错")
	}
}

// TestLatestReleaseMissingAsset 守：Release 存在但没有 taoapi.exe 资产。
//
// 这时 AssetURL 为空 —— 调用方必须能识别"没有可下载的资产"，
// 而不是拿空 URL 去下载。
func TestLatestReleaseMissingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.1.5","assets":[{"name":"source.zip","size":10,"browser_download_url":"https://github.com/x","digest":"sha256:aa"}]}`))
	}))
	t.Cleanup(srv.Close)

	u := &Updater{APIBase: srv.URL, Repo: "o/r", Client: srv.Client()}
	rel, err := u.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if rel == nil {
		t.Fatal("应返回 release")
	}
	if rel.AssetURL != "" {
		t.Errorf("没有匹配资产时 AssetURL 应为空，实际 %q", rel.AssetURL)
	}
}

func TestCleanupOldNoopWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, AssetName)
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 没有 .old 时应安静返回，不报错。
	if _, err := CleanupOld(); err != nil {
		t.Errorf("无 .old 时不该报错: %v", err)
	}
}
