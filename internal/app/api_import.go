// api_import.go：面板导入账号（面向 GitHub 用户）。
//
// 委托人要求：发布到 GitHub 后，别人下载软件就能用，
// 不能要求人人都开命令行。因此面板提供「粘贴 JSON 导入」：
//
//	POST /api/accounts/import
//	{"accounts":[{"uid":"...","nickname":"...","accessToken":"...","refreshToken":"...","domain":"..."}]}
//
// 导入即 DPAPI 加密落盘并立即刷新一次额度（否则账号会被调度排除，
// 这是步骤⑦踩过的坑）。
//
// 🔴 红线：本文件的任何响应都【绝不】回显 token。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy.local/workbuddy-api/internal/auth"
	"workbuddy.local/workbuddy-api/internal/pool"
)

// importBodyMaxBytes 导入请求体上限。
// 50 KiB 足够一次导入几十个账号（每账号约 600 字节）。
const importBodyMaxBytes = 50 << 10

// importAccountItem 是导入请求里的单个账号。
type importAccountItem struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	EnterpriseID string `json:"enterpriseId"`
	Domain       string `json:"domain"`
}

type importBody struct {
	Accounts []importAccountItem `json:"accounts"`
}

type importResult struct {
	OK       int      `json:"ok"` // 成功导入数
	Failed   int      `json:"failed"`
	Errors   []string `json:"errors,omitempty"`
	Accounts []struct {
		UID      string `json:"uid"` // 掩码
		Nickname string `json:"nickname"`
		Credits  *int64 `json:"credits"` // 刷新后的额度（null=刷新失败）
		Error    string `json:"error,omitempty"`
	} `json:"accounts"`
}

// handleAccountImport 处理面板导入。
func handleAccountImport(deps Deps, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, openaiErrTypeInvalidRequest,
			"method_not_allowed", "只支持 POST")
		return
	}
	if deps.Accounts == nil || deps.Persister == nil {
		writeError(w, http.StatusServiceUnavailable, openaiErrTypeServer,
			"no_accounts", "账号功能未启用")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, importBodyMaxBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"read_body_failed", "读取请求体失败: "+err.Error())
		return
	}
	if len(body) > importBodyMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, openaiErrTypeInvalidRequest,
			"body_too_large", "请求体超过上限 50 KiB")
		return
	}

	var req importBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Accounts) == 0 {
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"empty_accounts", "accounts 为空")
		return
	}
	if len(req.Accounts) > 100 {
		writeError(w, http.StatusBadRequest, openaiErrTypeInvalidRequest,
			"too_many", "单次最多导入 100 个账号")
		return
	}

	out := importResult{Errors: []string{}}
	for i, item := range req.Accounts {
		item.UID = strings.TrimSpace(item.UID)
		item.AccessToken = strings.TrimSpace(item.AccessToken)
		if item.UID == "" || item.AccessToken == "" {
			out.Failed++
			out.Errors = append(out.Errors,
				fmt.Sprintf("第 %d 个账号缺少 uid 或 accessToken", i+1))
			continue
		}

		acct := &auth.Account{
			UID:          item.UID,
			Nickname:     item.Nickname,
			EnterpriseID: item.EnterpriseID,
			Domain:       item.Domain,
			AccessToken:  item.AccessToken,
			RefreshToken: item.RefreshToken,
			Status:       pool.StatusNormal,
		}
		deps.Accounts.Put(acct)
		out.OK++
	}

	// 落盘（含已存在账号的更新）
	if err := deps.Persister.Save(deps.Accounts); err != nil {
		writeError(w, http.StatusInternalServerError, openaiErrTypeServer,
			"save_failed", "保存账号失败: "+err.Error())
		return
	}

	// 立即刷新额度（新账号不刷新会被调度排除）
	if deps.WBClient != nil {
		for _, item := range req.Accounts {
			if item.UID == "" || item.AccessToken == "" {
				continue
			}
			a, ok := deps.Accounts.Get(item.UID)
			if !ok {
				continue
			}
			p := providerForAccount(deps, a)
			if p == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), refreshTimeout)
			res, err := p.Credit(ctx, "")
			cancel()

			entry := struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
				Credits  *int64 `json:"credits"`
				Error    string `json:"error,omitempty"`
			}{UID: auth.MaskUID(a.UID), Nickname: a.Nickname}

			if err != nil {
				entry.Error = err.Error()
				out.Failed++
				out.Errors = append(out.Errors,
					a.Nickname+" 刷新额度失败: "+err.Error())
			} else {
				deps.Accounts.Mutate(a.UID, func(x *auth.Account) bool {
					applyCreditResult(x, res, time.Now())
					return true
				})
				v := res.Remaining
				entry.Credits = v
			}
			out.Accounts = append(out.Accounts, entry)
		}
		// 刷新结果再落盘一次
		_ = deps.Persister.Save(deps.Accounts)
	}

	writeJSON(w, http.StatusOK, out)
}
