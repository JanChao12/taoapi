package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 默认更新源：本项目的公开仓库。
const (
	DefaultAPIBase = "https://api.github.com"
	DefaultRepo    = "tao1677103724/taoapi"

	// AssetName 是 Release 里可执行资产的固定名。
	//
	// 🔴 写死而不是取"第一个资产"：Release 里通常还有源码包
	//	（zip/tar.gz）、校验文件等，取第一个是**猜测**，
	//	猜错就会下载到一个压缩包并试图替换 exe。
	AssetName = "taoapi.exe"
)

// 下载限制。
const (
	// maxAssetBytes 是允许下载的资产上限（64 MiB）。
	//
	// 本项目 exe 约 12 MB。设上限是为了防"上游被换成超大文件"
	// 导致的磁盘塞满与内存耗尽（我们流式写盘，但仍要有个界）。
	maxAssetBytes = 64 << 20

	// httpTimeout 覆盖"连上 + 读完"的总时长。
	httpTimeout = 5 * time.Minute
)

// Release 是一次更新检查的结果。
type Release struct {
	Tag       string // 原始 tag，如 v0.1.5
	Version   string // 规范化版本，如 0.1.5
	AssetName string
	AssetURL  string // browser_download_url
	Size      int64  // 资产字节数（GitHub 提供）
	Digest    string // `sha256:...`
}

// Updater 执行更新检查与下载。
type Updater struct {
	// APIBase 默认 DefaultAPIBase；测试注入假服务器。
	APIBase string
	Repo    string
	Client  *http.Client

	// validateURL 校验下载地址；nil 时用 ValidateAssetURL（生产默认）。
	//
	// 之所以做成字段而不是全局变量：测试需要一个"允许 http"的替身
	// （httptest 只能起 http 服务器），而全局变量会让并行测试互相干扰、
	// 也可能被生产代码误设。字段注入的作用域清晰得多。
	validateURL func(string) error
}

// assetURLValidator 返回本 Updater 使用的地址校验器。
func (u *Updater) assetURLValidator() func(string) error {
	if u.validateURL != nil {
		return u.validateURL
	}
	return ValidateAssetURL
}

// NewUpdater 返回使用生产默认值的 Updater。
func NewUpdater() *Updater {
	return &Updater{
		APIBase: DefaultAPIBase,
		Repo:    DefaultRepo,
		Client:  &http.Client{Timeout: httpTimeout},
	}
}

// gitHubRelease 是 GitHub API 响应里我们用到的字段（只声明需要的）。
type gitHubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// LatestRelease 查询最新 Release。
//
// 找不到 Release（仓库还没有任何发布）时返回 (nil, nil) ——
// **不是错误**：这是"尚未发布过"的正常状态，面板应显示"暂无可用更新"
// 而不是报错。区分这两种情况对用户体验很重要。
func (u *Updater) LatestRelease(ctx context.Context) (*Release, error) {
	base := u.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	repo := u.Repo
	if repo == "" {
		repo = DefaultRepo
	}

	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(base, "/"), repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("update：构造请求失败: %w", err)
	}
	// GitHub API 要求 User-Agent。
	req.Header.Set("User-Agent", "TAOAPI-updater")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update：查询更新失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 还没有任何 Release。读一小段 body 以便排错，但不当成错误。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("update：查询更新返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var gr gitHubRelease
	// 限制读取体大小，防异常响应打爆内存。
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gr); err != nil {
		return nil, fmt.Errorf("update：解析更新信息失败: %w", err)
	}

	rel := &Release{
		Tag:     gr.TagName,
		Version: NormalizeTag(gr.TagName),
	}
	for _, a := range gr.Assets {
		if a.Name == AssetName {
			rel.AssetName = a.Name
			rel.AssetURL = a.URL
			rel.Size = a.Size
			rel.Digest = a.Digest
			break
		}
	}
	return rel, nil
}

// ValidateAssetURL 校验下载地址是否可信。
//
// 🔴 这是本包的安全闸之一。规则：
//   - 必须是 **https**（http 明文可被中间人替换 exe）；
//   - 主机必须在**白名单**内。
//
// 为什么主机也要限死：地址来自 API 响应，而 API 响应理论上可被
// 上游/中间层改写。把"运行从哪来的字节"限制在固定主机上，
// 能把攻击面收窄到"那个主机被攻破"，而不是"任何一个 URL"。
//
// 白名单按后缀匹配（`objects.githubusercontent.com` 这类会带子域）。
func ValidateAssetURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("update：下载地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("update：下载地址无法解析: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("update：下载地址必须是 https，实际 %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	for _, ok := range []string{
		"github.com",
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
	} {
		if host == ok || strings.HasSuffix(host, "."+ok) {
			return nil
		}
	}
	return fmt.Errorf("update：下载地址主机不在白名单内: %q", host)
}

func (u *Updater) httpClient() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: httpTimeout}
}

// DownloadResult 是一次成功下载的结果。
type DownloadResult struct {
	Path   string // 下载到的临时文件路径
	Bytes  int64
	SHA256 string // 实际算出的 hex
}

// DownloadAsset 下载资产到 destDir，并**强制校验 digest**。
//
// 流程（任何一步失败都清理临时文件，不留下半成品）：
//  1. 校验 URL（https + 主机白名单）
//  2. 校验 digest 格式（缺失即拒绝 —— 见 ParseDigest 的说明）
//  3. 流式下载到 `destDir/<name>.new`，边写边算 sha256，超出上限即中止
//  4. 比对哈希：不一致 ⇒ 删除临时文件并报错
//
// 🔴 绝不"边下边替换"：必须完整下载 + 校验通过，才交给替换环节。
func (u *Updater) DownloadAsset(ctx context.Context, rel *Release, destDir string) (*DownloadResult, error) {
	if rel == nil {
		return nil, fmt.Errorf("update：没有可下载的版本信息")
	}
	if err := u.assetURLValidator()(rel.AssetURL); err != nil {
		return nil, err
	}
	_, wantHex, err := ParseDigest(rel.Digest)
	if err != nil {
		return nil, err
	}
	// 上游声明的体积也要过一遍上限（防止 Content-Length 巨大时白下载）。
	if rel.Size > maxAssetBytes {
		return nil, fmt.Errorf("update：资产体积 %d 超过上限 %d", rel.Size, maxAssetBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("update：构造下载请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "TAOAPI-updater")

	resp, err := u.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update：下载失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update：下载返回 HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("update：创建目录失败: %w", err)
	}
	tmpPath := filepath.Join(destDir, AssetName+".new")

	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("update：创建临时文件失败: %w", err)
	}

	// 失败时清理：成功路径会显式把 cleanup 置 false。
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	// io.LimitReader 多读 1 字节：若真能读到第 maxAssetBytes+1 字节，
	// 说明对方比声明的大 ⇒ 判定超限（而不是静默截断）。
	limited := io.LimitReader(resp.Body, maxAssetBytes+1)
	n, err := io.Copy(io.MultiWriter(f, h), limited)
	if err != nil {
		return nil, fmt.Errorf("update：写入失败: %w", err)
	}
	if n > maxAssetBytes {
		return nil, fmt.Errorf("update：下载内容超过上限 %d 字节", maxAssetBytes)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("update：落盘失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("update：关闭文件失败: %w", err)
	}

	gotHex := hex.EncodeToString(h.Sum(nil))
	if gotHex != wantHex {
		return nil, fmt.Errorf("update：校验失败，下载内容与官方校验值不符\n"+
			"  期望 sha256:%s\n  实际 sha256:%s\n"+
			"（文件已删除，未做任何替换）", wantHex, gotHex)
	}

	cleanup = false
	return &DownloadResult{Path: tmpPath, Bytes: n, SHA256: gotHex}, nil
}
