package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type accountCreditBalance struct {
	Balance   float64 `json:"balance"` // User-facing Credits, not upstream microcredits.
	CheckedAt int64   `json:"checkedAt"`
}

type accountCreditState struct {
	done       chan struct{}
	pending    bool
	value      *accountCreditBalance
	err        error
	validUntil time.Time
}

var accountBalanceSlots = make(chan struct{}, 3)

// The local acc_... ID is not Cline's user ID. Resolve /users/me first, as
// the official ClineAccountService does, then query the personal credit balance.
func fetchAccountBalance(ctx context.Context, acc *Account) (*accountCreditBalance, error) {
	ctx, stop := context.WithTimeout(ctx, 45*time.Second)
	defer stop()
	select {
	case accountBalanceSlots <- struct{}{}:
		defer func() { <-accountBalanceSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("余额查询等待超时，请稍后重试")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return nil, fmt.Errorf("账号凭据刷新失败，请检查账号登录状态")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var user struct {
		ID string `json:"id"`
	}
	if err := accountBalanceGET(ctx, acc, &token, "/users/me", &user); err != nil {
		return nil, err
	}
	if user.ID == "" {
		return nil, fmt.Errorf("官方接口未返回用户 ID")
	}
	var credits struct {
		Balance *float64 `json:"balance"`
	}
	if err := accountBalanceGET(ctx, acc, &token, "/users/"+url.PathEscape(user.ID)+"/balance", &credits); err != nil {
		return nil, err
	}
	if credits.Balance == nil {
		return nil, fmt.Errorf("官方接口未返回 Credit 余额")
	}
	// Match Cline's formatCreditsBalance: 1 Credit = 10,000 microcredits.
	// Convert before caching so both the account list and details use Credits.
	return &accountCreditBalance{Balance: *credits.Balance / 10000, CheckedAt: time.Now().UnixMilli()}, nil
}

func accountBalanceGET(ctx context.Context, acc *Account, token *string, path string, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, clineAPIBase+path, nil)
		if err != nil {
			return fmt.Errorf("余额查询请求创建失败")
		}
		req.Header = clineHeaders(*token, "")
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("余额查询连接失败或超时，请稍后重试")
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			refreshed, err := accountToken(acc, true, *token)
			if err != nil {
				return fmt.Errorf("账号凭据刷新失败，请检查账号登录状态")
			}
			*token = refreshed
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("官方余额查询失败（HTTP %d）", resp.StatusCode)
		}
		var envelope struct {
			Success *bool           `json:"success"`
			Data    json.RawMessage `json:"data"`
			Error   json.RawMessage `json:"error"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope)
		resp.Body.Close()
		if err != nil || (envelope.Success != nil && !*envelope.Success) || (len(envelope.Error) > 0 && string(envelope.Error) != "null" && string(envelope.Error) != `""`) || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return fmt.Errorf("官方余额接口响应异常，请稍后重试")
		}
		if json.Unmarshal(envelope.Data, out) != nil {
			return fmt.Errorf("官方余额接口数据格式不正确")
		}
		return nil
	}
	return fmt.Errorf("账号认证失败，请重新登录")
}

func cachedAccountBalance(ctx context.Context, acc *Account, force bool) (*accountCreditBalance, error) {
	poolMu.Lock()
	if cached := acc.credits; cached != nil {
		if cached.pending {
			poolMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-cached.done:
				return cached.value, cached.err
			}
		}
		if !force && time.Now().Before(cached.validUntil) {
			poolMu.Unlock()
			return cached.value, cached.err
		}
	}
	state := &accountCreditState{done: make(chan struct{}), pending: true}
	acc.credits = state
	poolMu.Unlock()
	value, err := fetchAccountBalance(ctx, acc)
	poolMu.Lock()
	state.value, state.err, state.pending = value, err, false
	ttl := time.Minute
	if err != nil {
		ttl = 10 * time.Second
	}
	state.validUntil = time.Now().Add(ttl)
	close(state.done)
	poolMu.Unlock()
	return value, err
}

func handleAdminAccountBalance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	id := r.URL.Query().Get("accountId")
	if id == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}
	acc := getAccountByID(id)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}
	value, err := cachedAccountBalance(r.Context(), acc, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: value})
}
