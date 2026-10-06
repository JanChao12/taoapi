package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"workbuddy.local/workbuddy-api/internal/storage"
)

// diskFile 是落盘的整体结构。
//
// ⚠️ 分层设计：只有每个账号的 Secrets 段是加密的，其余字段【明文】存。
// 理由：面板与诊断需要在【不解密凭据】的前提下读取账号状态
// （UID/昵称/额度/封号状态）。整文件加密会让面板必须先解密才能显示状态，
// 既无必要，也模糊了"读状态"与"读凭据"的边界。
type diskFile struct {
	Version  int        `json:"version"`
	Accounts []diskAcct `json:"accounts"`
}

// accountsFileVersion 是账号文件的格式版本。
const accountsFileVersion = 1

// diskAcct 是单个账号的落盘形式。
type diskAcct struct {
	// ── 非秘密（明文）──
	UID string `json:"uid"`

	// Platform 所属平台（"cn"/"intl"）；老文件没有这个字段，
	// 读出后按国内版处理（见 Account.Platform 的说明）。
	Platform string `json:"platform,omitempty"`

	Nickname       string     `json:"nickname,omitempty"`
	Domain         string     `json:"domain,omitempty"`
	ManualDisabled bool       `json:"manual_disabled,omitempty"`
	Status         string     `json:"status,omitempty"`
	StatusReason   string     `json:"status_reason,omitempty"`
	StatusUntil    string     `json:"status_until,omitempty"` // RFC3339
	LastObservedAt string     `json:"last_observed_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	CheckinDay     string     `json:"checkin_day,omitempty"`
	CheckinAt      string     `json:"checkin_at,omitempty"`
	Credit         diskCredit `json:"credit"`

	// ── 秘密（加密）──
	Secrets string `json:"secrets"` // storage.envelope 的 JSON
}

// diskCredit 是额度快照的落盘形式。
type diskCredit struct {
	Known     bool              `json:"known"`
	Remaining int64             `json:"remaining"`
	At        string            `json:"at,omitempty"`
	Packages  []PackageSnapshot `json:"packages,omitempty"`
}

// secrets 是要加密的部分。
type secrets struct {
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	EnterpriseID   string `json:"enterprise_id,omitempty"`
	TokenExpiresAt int64  `json:"token_expires_at,omitempty"`
}

// Persister 负责账号的加解密落盘。
type Persister struct {
	path  string
	codec storage.Codec
}

// NewPersister 创建持久化器。
func NewPersister(path string, codec storage.Codec) *Persister {
	return &Persister{path: path, codec: codec}
}

// Path 返回文件路径。
func (p *Persister) Path() string { return p.path }

// Exists 报告账号文件是否存在。
func (p *Persister) Exists() bool {
	_, err := os.Stat(p.path)
	return err == nil
}

// Load 读取全部账号。文件不存在时返回空仓库而不是错误。
func (p *Persister) Load() (*Store, error) {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewStore(), nil
		}
		return nil, fmt.Errorf("读取账号文件失败: %w", err)
	}

	var df diskFile
	if err := json.Unmarshal(raw, &df); err != nil {
		return nil, fmt.Errorf("解析账号文件失败: %w", err)
	}
	if df.Version != accountsFileVersion {
		return nil, fmt.Errorf("账号文件版本 %d 不受支持（本程序支持 %d）",
			df.Version, accountsFileVersion)
	}

	st := NewStore()
	// 单条损坏不影响其余账号 —— 否则一个坏号会让整个服务起不来
	var firstErr error
	for _, da := range df.Accounts {
		a, err := p.decode(da)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("账号 %s: %w", MaskUID(da.UID), err)
			}
			continue
		}
		st.Put(a)
	}

	// 全部失败且确实有账号时才报错
	if st.Len() == 0 && len(df.Accounts) > 0 && firstErr != nil {
		return nil, firstErr
	}
	return st, firstErr
}

// Save 原子写入全部账号（明文状态 + 各自加密的凭据，同一次原子替换）。
//
// ⚠️ 两者写进同一个文件的同一次原子替换里，避免"状态写了、凭据没写"的不一致。
//
// 🔴 2026-10-07 修既存数据竞争：本方法原来在**无锁**情况下遍历
// `st.List()` 返回的**共享指针**并序列化**全部字段**，而 Save 由
// `persistAccounts`（failover.go:329）在**每个失败请求后**调用 ——
// 也就是说它与 `refreshCredits` 的 `Credit.Packages[:0]+append`
// 并发时，可能把**正在被原地修改的切片**写进磁盘。
//
// ⚠️ 为什么这是最严重的一处：其它竞争只影响单次响应，重启即消失；
// **而这里写坏的文件会在下次启动时被读回** —— 竞争被"固化"了。
//
// 现在改为在**锁内**取一份值拷贝（`ListSnapshots`）再序列化，
// 得到的是一份自洽的快照。
//
// ⚠️ 这**不**保证"磁盘内容等于某个确定时刻的全局状态"（多个账号之间
// 仍可能来自略微不同的时刻），但**保证每个账号自身的字段组合自洽** ——
// 那正是会产生损坏数据的那一面。
func (p *Persister) Save(st *Store) error {
	df := diskFile{Version: accountsFileVersion}

	// 锁内值拷贝 —— 不拿共享指针
	snaps := st.ListSnapshots()
	for i := range snaps {
		da, err := p.encode(&snaps[i])
		if err != nil {
			return fmt.Errorf("账号 %s: %w", MaskUID(snaps[i].UID), err)
		}
		df.Accounts = append(df.Accounts, da)
	}

	raw, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账号失败: %w", err)
	}

	if err := storage.AtomicWrite(p.path, raw, 0o600); err != nil {
		return fmt.Errorf("写入账号文件失败: %w", err)
	}
	return nil
}

// encode 把账号拆成"明文状态 + 加密凭据"。
func (p *Persister) encode(a *Account) (diskAcct, error) {
	sec, err := json.Marshal(secrets{
		AccessToken:    a.AccessToken,
		RefreshToken:   a.RefreshToken,
		EnterpriseID:   a.EnterpriseID,
		TokenExpiresAt: a.TokenExpiresAt,
	})
	if err != nil {
		return diskAcct{}, fmt.Errorf("序列化凭据失败: %w", err)
	}

	env, err := storage.NewEnvelope(p.codec, sec)
	if err != nil {
		// 🔴 绝不退回明文
		return diskAcct{}, fmt.Errorf("加密凭据失败（已放弃保存该账号）: %w", err)
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return diskAcct{}, fmt.Errorf("序列化凭据信封失败: %w", err)
	}

	return diskAcct{
		UID:            a.UID,
		Platform:       NormalizePlatform(a.Platform),
		Nickname:       a.Nickname,
		Domain:         a.Domain,
		ManualDisabled: a.ManualDisabled,
		Status:         string(a.Status.Normalize()),
		StatusReason:   a.StatusReason,
		StatusUntil:    rfc3339(a.StatusUntil),
		LastObservedAt: rfc3339(a.LastObservedAt),
		LastError:      a.LastError,
		CheckinDay:     a.CheckinDay,
		CheckinAt:      rfc3339(a.CheckinAt),
		Credit: diskCredit{
			Known:     a.Credit.Known,
			Remaining: a.Credit.Remaining,
			At:        rfc3339(a.Credit.At),
			Packages:  a.Credit.Packages,
		},
		Secrets: string(envJSON),
	}, nil
}

// decode 还原账号。
func (p *Persister) decode(da diskAcct) (*Account, error) {
	if da.UID == "" {
		return nil, fmt.Errorf("缺少 uid")
	}

	var env storage.Envelope
	if err := json.Unmarshal([]byte(da.Secrets), &env); err != nil {
		return nil, fmt.Errorf("解析凭据信封失败: %w", err)
	}

	plain, err := env.Open(p.codec)
	if err != nil {
		return nil, fmt.Errorf("凭据解密失败: %w", err)
	}

	var sec secrets
	if err := json.Unmarshal(plain, &sec); err != nil {
		return nil, fmt.Errorf("解析凭据失败: %w", err)
	}

	return &Account{
		UID: da.UID,
		// 老文件没有 platform 字段 ⇒ 归一为国内版
		// （接入国际版之前的账号都是国内的；见 Account.Platform 的说明）
		Platform:       NormalizePlatform(da.Platform),
		Nickname:       da.Nickname,
		EnterpriseID:   sec.EnterpriseID,
		Domain:         da.Domain,
		AccessToken:    sec.AccessToken,
		RefreshToken:   sec.RefreshToken,
		TokenExpiresAt: sec.TokenExpiresAt,
		ManualDisabled: da.ManualDisabled,
		Status:         poolStatus(da.Status),
		StatusReason:   da.StatusReason,
		StatusUntil:    parseRFC3339(da.StatusUntil),
		LastObservedAt: parseRFC3339(da.LastObservedAt),
		LastError:      da.LastError,
		CheckinDay:     da.CheckinDay,
		CheckinAt:      parseRFC3339(da.CheckinAt),
		Credit: CreditSnapshot{
			Known:     da.Credit.Known,
			Remaining: da.Credit.Remaining,
			At:        parseRFC3339(da.Credit.At),
			Packages:  da.Credit.Packages,
		},
	}, nil
}

// AccountsPath 返回默认账号文件路径。
func AccountsPath() string {
	return filepath.Join(defaultDir(), "accounts.json")
}

// defaultDir 返回数据目录。
func defaultDir() string {
	if d := os.Getenv("WBAPI_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".wbapi"
	}
	return filepath.Join(home, ".wbapi")
}
