package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type account struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"account_id"`
	APIToken  string `json:"api_token"`
	Enabled   bool   `json:"enabled"`
}
type accountStats struct {
	Requests      uint64     `json:"requests"`
	Failures      uint64     `json:"failures"`
	LastStatus    int        `json:"last_status"`
	LastUsed      *time.Time `json:"last_used"`
	CooldownUntil *time.Time `json:"cooldown_until"`
}
type accountView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"account_id"`
	TokenHint string `json:"token_hint"`
	Enabled   bool   `json:"enabled"`
	Next      bool   `json:"next"`
	accountStats
}
type accountPool struct {
	mu       sync.Mutex
	accounts []account
	stats    map[string]*accountStats
	cursor   int
	file     string
}

func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure random unavailable")
	}
	return hex.EncodeToString(b[:])
}
func validateAccount(a account) error {
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > 128 {
		return invalid("账号名称不能为空，且不能超过 128 字节")
	}
	if !accountIDPattern.MatchString(a.AccountID) {
		return invalid("Account ID 必须为 32 位十六进制字符")
	}
	if !validSecret(a.APIToken) || len(a.APIToken) > 4096 {
		return invalid("API Token 不能为空、包含空白或超过 4096 字节")
	}
	return nil
}
func newAccountPool(c Config) (*accountPool, error) {
	p := &accountPool{file: c.AccountsFile, stats: map[string]*accountStats{}, accounts: []account{}}
	if p.file != "" {
		file, err := os.Open(p.file)
		if err == nil {
			defer file.Close()
			data, err := readLimited(file, 1<<20)
			if err != nil {
				return nil, fmt.Errorf("cannot read account store")
			}
			var disk struct {
				Version  int       `json:"version"`
				Accounts []account `json:"accounts"`
			}
			if json.Unmarshal(data, &disk) != nil || disk.Version != 1 || disk.Accounts == nil || len(disk.Accounts) > 100 {
				return nil, fmt.Errorf("invalid account store; fix data/accounts.json before starting")
			}
			ids, accounts := map[string]bool{}, map[string]bool{}
			for _, a := range disk.Accounts {
				if validateAccount(a) != nil || len(a.ID) != 64 || ids[a.ID] || accounts[strings.ToLower(a.AccountID)] {
					return nil, fmt.Errorf("invalid or duplicate account in store")
				}
				ids[a.ID], accounts[strings.ToLower(a.AccountID)] = true, true
			}
			p.accounts = disk.Accounts
			if err := os.Chmod(p.file, 0600); err != nil {
				return nil, fmt.Errorf("cannot secure account store permissions: %w", err)
			}
			return p, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("open account store: %w", err)
		}
	}
	if err := p.save(p.accounts); err != nil {
		return nil, err
	}
	return p, nil
}

// Save before publishing changes, so failed writes never alter the live pool.
func (p *accountPool) save(accounts []account) error {
	if p.file == "" {
		return nil
	}
	dir := filepath.Dir(p.file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create account store directory")
	}
	data, err := json.MarshalIndent(struct {
		Version  int       `json:"version"`
		Accounts []account `json:"accounts"`
	}{1, accounts}, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".accounts-*")
	if err != nil {
		return fmt.Errorf("cannot write account store")
	}
	defer os.Remove(file.Name())
	failed := func() error { file.Close(); return fmt.Errorf("cannot write account store") }
	if err = file.Chmod(0600); err != nil {
		return failed()
	}
	if _, err = file.Write(append(data, '\n')); err != nil {
		return failed()
	}
	if err = file.Sync(); err != nil {
		return failed()
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("cannot close account store")
	}
	if err = os.Rename(file.Name(), p.file); err != nil {
		return fmt.Errorf("cannot replace account store")
	}
	return nil
}
func (p *accountPool) available(a account, now time.Time) bool {
	stats := p.stats[a.ID]
	return a.Enabled && (stats == nil || stats.CooldownUntil == nil || !now.Before(*stats.CooldownUntil))
}
func (p *accountPool) views() []accountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := ""
	now := time.Now()
	for n := 0; n < len(p.accounts); n++ {
		a := p.accounts[(p.cursor+n)%len(p.accounts)]
		if p.available(a, now) {
			next = a.ID
			break
		}
	}
	views := make([]accountView, 0, len(p.accounts))
	for _, a := range p.accounts {
		hint := "已设置"
		if len(a.APIToken) > 8 {
			hint = "••••" + a.APIToken[len(a.APIToken)-4:]
		}
		v := accountView{ID: a.ID, Name: a.Name, AccountID: a.AccountID, TokenHint: hint, Enabled: a.Enabled, Next: a.ID == next}
		if stats := p.stats[a.ID]; stats != nil {
			v.accountStats = *stats
		}
		views = append(views, v)
	}
	return views
}
func (p *accountPool) selectAccount(w http.ResponseWriter) (account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var earliest *time.Time
	for n := 0; n < len(p.accounts); n++ {
		index := (p.cursor + n) % len(p.accounts)
		a := p.accounts[index]
		if p.available(a, now) {
			p.cursor = (index + 1) % len(p.accounts)
			return a, nil
		}
		if a.Enabled {
			if stats := p.stats[a.ID]; stats != nil && stats.CooldownUntil != nil && (earliest == nil || stats.CooldownUntil.Before(*earliest)) {
				earliest = stats.CooldownUntil
			}
		}
	}
	if earliest != nil {
		seconds := int(time.Until(*earliest).Seconds()) + 1
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	return account{}, &apiFailure{503, "没有可用账号，请在管理页面添加、启用账号，或等待冷却结束"}
}
func (p *accountPool) record(a account, resp *http.Response, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	found := false
	for _, current := range p.accounts {
		if current.ID == a.ID && current.APIToken == a.APIToken && current.AccountID == a.AccountID {
			found = true
			break
		}
	}
	if !found {
		return
	}
	st := p.stats[a.ID]
	if st == nil {
		st = &accountStats{}
		p.stats[a.ID] = st
	}
	now := time.Now()
	st.Requests++
	st.LastUsed = &now
	st.LastStatus = 0
	if resp != nil {
		st.LastStatus = resp.StatusCode
	}
	if err == nil && resp != nil && resp.StatusCode < 300 {
		return
	}
	st.Failures++
	var delay time.Duration
	switch {
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		delay = 15 * time.Second
	case resp.StatusCode == 429:
		delay = time.Minute
		retry := resp.Header.Get("Retry-After")
		if seconds, e := strconv.Atoi(retry); e == nil && seconds > 0 && seconds <= 86400 {
			delay = time.Duration(seconds) * time.Second
		} else if date, e := http.ParseTime(retry); e == nil && date.After(now) {
			delay = date.Sub(now)
		}
		if delay > 24*time.Hour {
			delay = 24 * time.Hour
		}
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		delay = time.Minute
	case resp.StatusCode >= 500:
		delay = 15 * time.Second
	}
	if delay > 0 {
		until := now.Add(delay)
		if st.CooldownUntil == nil || until.After(*st.CooldownUntil) {
			st.CooldownUntil = &until
		}
	}
}

type accountBindingKey struct{}
type accountBinding struct{ selected *account }

func (s *Server) upstream(ctx context.Context, w http.ResponseWriter, path, contentType, accept string, body []byte) (*http.Response, error) {
	binding, _ := ctx.Value(accountBindingKey{}).(*accountBinding)
	var a account
	if binding != nil && binding.selected != nil {
		a = *binding.selected
	} else {
		var err error
		a, err = s.accounts.selectAccount(w)
		if err != nil {
			return nil, err
		}
		if binding != nil {
			binding.selected = &a
		}
	}
	base := "https://api.cloudflare.com/client/v4/accounts/" + a.AccountID + "/ai"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIToken)
	req.Header.Set("Content-Type", contentType)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	var step *upstreamTrace
	if trace := traceFrom(ctx); trace != nil {
		step = trace.beginUpstream(a, path, contentType, body, req.Header)
	}
	resp, err := s.client.Do(req)
	if step != nil {
		step.gotResponse(resp, err)
	}
	s.accounts.record(a, resp, err)
	return resp, err
}
func redactAccountError(ctx context.Context, message string) string {
	if binding, ok := ctx.Value(accountBindingKey{}).(*accountBinding); ok && binding.selected != nil {
		return strings.ReplaceAll(message, binding.selected.APIToken, "[redacted]")
	}
	return message
}

// Mutations are serialized with selection and committed atomically to disk.
type accountUpdate struct {
	Name      *string `json:"name"`
	AccountID *string `json:"account_id"`
	APIToken  *string `json:"api_token"`
	Enabled   *bool   `json:"enabled"`
}

func (p *accountPool) update(id string, patch accountUpdate, remove bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	items := append([]account{}, p.accounts...)
	index := -1
	for i, a := range items {
		if a.ID == id {
			index = i
			break
		}
	}
	if id != "" && index < 0 {
		return "", &apiFailure{404, "账号不存在"}
	}
	if remove {
		items = append(items[:index], items[index+1:]...)
	} else {
		a := account{ID: randomID(), Enabled: true}
		if index >= 0 {
			a = items[index]
		} else if len(items) >= 100 {
			return "", invalid("最多支持 100 个账号")
		}
		if patch.Name != nil {
			a.Name = strings.TrimSpace(*patch.Name)
		}
		if patch.AccountID != nil {
			a.AccountID = strings.ToLower(strings.TrimSpace(*patch.AccountID))
		}
		if patch.APIToken != nil && *patch.APIToken != "" {
			a.APIToken = *patch.APIToken
		}
		if patch.Enabled != nil {
			a.Enabled = *patch.Enabled
		}
		if err := validateAccount(a); err != nil {
			return "", err
		}
		for i, other := range items {
			if i != index && strings.EqualFold(other.AccountID, a.AccountID) {
				return "", &apiFailure{409, "这个 Account ID 已在账号池中"}
			}
		}
		id = a.ID
		if index >= 0 {
			items[index] = a
		} else {
			items = append(items, a)
		}
	}
	if err := p.save(items); err != nil {
		return "", &apiFailure{500, "账号保存失败，请检查 data 目录的写入权限"}
	}
	p.accounts = items
	if len(items) > 0 {
		p.cursor %= len(items)
	} else {
		p.cursor = 0
	}
	if remove {
		delete(p.stats, id)
	} else if patch.APIToken != nil && *patch.APIToken != "" || patch.AccountID != nil || patch.Enabled != nil && *patch.Enabled {
		if stats := p.stats[id]; stats != nil {
			stats.CooldownUntil = nil
		}
	}
	return id, nil
}
