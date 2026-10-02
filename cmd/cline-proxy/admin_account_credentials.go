package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	errCredentialAccountMissing = errors.New("account no longer exists")
	errCredentialEmailMismatch  = errors.New("authenticated email does not match account")
)

type replacementCredentials struct {
	email        string
	accessToken  string
	refreshToken string
	expiresAt    int64
}

// Update only the credential fields of the existing record. In particular,
// account ID, creation time, usage counters, and the disabled flag survive.
func replaceAccountCredentials(accountID string, next replacementCredentials) error {
	if next.email == "" || next.accessToken == "" || next.refreshToken == "" {
		return fmt.Errorf("新凭据缺少账号邮箱或 Token")
	}
	for {
		p := loadPool()
		poolMu.Lock()
		var acc *Account
		for _, candidate := range p.Accounts {
			if candidate != nil && candidate.AccountID == accountID {
				acc = candidate
				break
			}
		}
		if acc == nil {
			poolMu.Unlock()
			return errCredentialAccountMissing
		}
		if !strings.EqualFold(strings.TrimSpace(acc.Email), strings.TrimSpace(next.email)) {
			poolMu.Unlock()
			return errCredentialEmailMismatch
		}
		if acc.refresh != nil {
			done := acc.refresh.done
			poolMu.Unlock()
			<-done
			continue
		}
		acc.AccessToken = "workos:" + next.accessToken
		acc.RefreshToken = next.refreshToken
		acc.ExpiresAt = next.expiresAt
		acc.Status = "active"
		acc.credits = nil
		acc.tokenSavePending = false
		if err := savePoolLocked(); err != nil {
			// Upstream may already have rotated the token. Keep the new credential
			// in memory and retry persistence before the next use.
			acc.tokenSavePending = true
			poolMu.Unlock()
			return fmt.Errorf("保存新凭据失败，请检查账号池文件是否可写: %w", err)
		}
		poolMu.Unlock()
		return nil
	}
}

// A manually supplied refresh token does not prove which account owns it.
// Resolve /users/me with the new access token before replacing any record.
func emailForAccessToken(ctx context.Context, accessToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clineAPIBase+"/users/me", nil)
	if err != nil {
		return "", fmt.Errorf("创建账号验证请求失败")
	}
	req.Header = clineHeaders("workos:"+accessToken, "")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("验证账号邮箱时连接官方接口失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("验证账号邮箱失败（HTTP %d）", resp.StatusCode)
	}
	var envelope struct {
		Success *bool `json:"success"`
		Data    struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil || (envelope.Success != nil && !*envelope.Success) || envelope.Data.Email == "" {
		return "", fmt.Errorf("官方接口未返回可核对的账号邮箱")
	}
	return envelope.Data.Email, nil
}

func replaceOAuthAccountCredentials(accountID string, cline *clineAuthResp) (string, error) {
	if cline == nil || cline.Data.AccessToken == "" {
		return "", fmt.Errorf("OAuth 未返回可用的访问凭据")
	}
	var email string
	if cline.Data.UserInfo != nil {
		email = cline.Data.UserInfo.Email
	}
	if email == "" {
		var err error
		email, err = emailForAccessToken(context.Background(), cline.Data.AccessToken)
		if err != nil {
			return "", err
		}
	}
	err := replaceAccountCredentials(accountID, replacementCredentials{
		email: email, accessToken: cline.Data.AccessToken,
		refreshToken: cline.Data.RefreshToken, expiresAt: parseExpiry(cline.Data.ExpiresAt) - 60000,
	})
	return email, err
}

// POST /admin/api/accounts/credentials  {accountId, refreshToken}
func handleAdminAccountCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var req struct {
		AccountID    string `json:"accountId"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := readJSONBody(r, &req); err != nil || req.AccountID == "" || strings.TrimSpace(req.RefreshToken) == "" || len(req.RefreshToken) > 8192 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "需要有效的账号 ID 和 Refresh Token"})
		return
	}
	if getAccountByID(req.AccountID) == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "目标账号不存在"})
		return
	}
	refreshed, err := refreshClineToken(strings.TrimSpace(req.RefreshToken))
	if err != nil {
		writeAPI(w, http.StatusFailedDependency, apiResponse{Error: balanceCredentialError(err).Error()})
		return
	}
	email, err := emailForAccessToken(r.Context(), refreshed.Data.AccessToken)
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if refreshed.Data.RefreshToken != "" {
		refreshToken = refreshed.Data.RefreshToken
	}
	if err != nil {
		writeAPI(w, http.StatusFailedDependency, apiResponse{Error: err.Error(), Data: map[string]any{"refreshToken": refreshToken}})
		return
	}
	err = replaceAccountCredentials(req.AccountID, replacementCredentials{
		email: email, accessToken: refreshed.Data.AccessToken,
		refreshToken: refreshToken, expiresAt: parseExpiry(refreshed.Data.ExpiresAt) - 60000,
	})
	if err != nil {
		writeCredentialUpdateError(w, err, refreshToken)
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"accountId": req.AccountID, "email": email}})
}

func writeCredentialUpdateError(w http.ResponseWriter, err error, refreshToken string) {
	response := apiResponse{Error: credentialUpdateMessage(err), Data: map[string]any{"refreshToken": refreshToken}}
	switch {
	case errors.Is(err, errCredentialAccountMissing):
		writeAPI(w, http.StatusNotFound, response)
	case errors.Is(err, errCredentialEmailMismatch):
		writeAPI(w, http.StatusConflict, response)
	default:
		writeAPI(w, http.StatusInternalServerError, response)
	}
}

func credentialUpdateMessage(err error) string {
	switch {
	case errors.Is(err, errCredentialAccountMissing):
		return "目标账号已删除，未更新凭据"
	case errors.Is(err, errCredentialEmailMismatch):
		return "新凭据登录的邮箱与目标账号不一致，未覆盖原账号"
	default:
		return err.Error()
	}
}
