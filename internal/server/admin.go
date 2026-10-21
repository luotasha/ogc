package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xynogen/ogc/internal/config"
)

type adminAccount struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Masked  string `json:"masked_key"`
	Model   string `json:"model"`
	Enabled bool   `json:"enabled"`
}

type adminSession struct {
	Hash      string `json:"hash"`
	SessionID string `json:"session_id"`
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
	LastSeen  int64  `json:"last_seen"`
}

func accountName(a config.Account, index int) string {
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	return fmt.Sprintf("账号 %d", index+1)
}

func maskKey(key string) string {
	if len(key) < 9 {
		return "••••••••"
	}
	return key[:4] + "••••" + key[len(key)-4:]
}

func (s *sessionRouter) Admin(w http.ResponseWriter, r *http.Request) {
	if !localRequest(r) {
		http.Error(w, "admin UI is available from localhost only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	_ = adminPage.Execute(w, nil)
}

func (s *sessionRouter) AdminAPI(w http.ResponseWriter, r *http.Request) {
	if !localRequest(r) {
		http.Error(w, "localhost only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && !sameOrigin(r) {
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	switch {
	case r.Method == http.MethodGet && path == "state":
		s.adminState(w)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "sessions/") && strings.HasSuffix(path, "/preview"):
		hash := strings.TrimSuffix(strings.TrimPrefix(path, "sessions/"), "/preview")
		s.adminSessionPreview(w, hash)
	case r.Method == http.MethodPost && path == "accounts":
		s.adminAddAccount(w, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "accounts/"):
		s.adminEditAccount(w, r, strings.TrimPrefix(path, "accounts/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "accounts/"):
		s.adminDeleteAccount(w, strings.TrimPrefix(path, "accounts/"))
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "sessions/"):
		s.adminMoveSession(w, r, strings.TrimPrefix(path, "sessions/"))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func localRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && subtle.ConstantTimeCompare([]byte(strings.ToLower(u.Host)), []byte(strings.ToLower(r.Host))) == 1
}

func (s *sessionRouter) adminState(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]adminAccount, 0, len(s.accounts))
	for i, a := range s.accounts {
		accounts = append(accounts, adminAccount{a.ID, accountName(a, i), maskKey(a.APIKey), a.Model, !a.Disabled})
	}
	discovered := discoverSessions(s.bindings)
	sessions := make([]adminSession, 0, len(s.bindings))
	for hash, b := range s.bindings {
		id := b.SessionID
		if id == "" {
			id = discovered[hash].ID
		}
		if id == "" {
			id = hash[:12] + "…"
		}
		lastSeen := b.LastSeen
		if discovered[hash].LastSeen > lastSeen {
			lastSeen = discovered[hash].LastSeen
		}
		sessions = append(sessions, adminSession{hash, id, b.Account, b.Model, lastSeen})
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastSeen == sessions[j].LastSeen {
			return sessions[i].SessionID < sessions[j].SessionID
		}
		return sessions[i].LastSeen > sessions[j].LastSeen
	})
	models := make([]string, 0, len(s.cfg.Models))
	for id := range s.cfg.Models {
		models = append(models, id)
	}
	sort.Strings(models)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"accounts": accounts, "sessions": sessions, "models": models, "editable": os.Getenv("OGC_API_KEYS") == ""})
}

type discoveredSession struct {
	ID       string
	LastSeen int64
}

func discoverSessions(wanted map[string]binding) map[string]discoveredSession {
	result := map[string]discoveredSession{}
	home, err := os.UserHomeDir()
	if err != nil {
		return result
	}
	root := filepath.Join(home, ".claude", "projects")
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".jsonl") {
			id := strings.TrimSuffix(info.Name(), ".jsonl")
			if len(id) == 36 && strings.Count(id, "-") == 4 {
				hash := digest(id)
				if _, ok := wanted[hash]; ok {
					result[hash] = discoveredSession{ID: id, LastSeen: info.ModTime().UnixMilli()}
				}
			}
		}
		return nil
	})
	return result
}

func (s *sessionRouter) adminSessionPreview(w http.ResponseWriter, hash string) {
	s.mu.Lock()
	b, ok := s.bindings[hash]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	id := b.SessionID
	if id == "" {
		id = discoverSessions(map[string]binding{hash: b})[hash].ID
	}
	preview := ""
	if id != "" {
		if home, err := os.UserHomeDir(); err == nil {
			matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", id+".jsonl"))
			if len(matches) > 0 {
				preview = sessionPreview(matches[0])
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"preview": preview})
}

const previewTailBytes int64 = 512 << 10

func sessionPreview(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - previewTailBytes
	if start < 0 {
		start = 0
	}
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := bytes.Split(data, []byte{'\n'})
	turns := make([]string, 0, 4)
	for i := len(lines) - 1; i >= 0 && len(turns) < 4; i-- {
		turn := previewTurn(lines[i])
		if turn != "" {
			turns = append(turns, turn)
		}
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return strings.Join(turns, "\n")
}

func previewTurn(line []byte) string {
	var row struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &row) != nil || row.IsSidechain || (row.Type != "user" && row.Type != "assistant") {
		return ""
	}
	var text string
	if json.Unmarshal(row.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(row.Message.Content, &blocks) != nil {
			return ""
		}
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == "text" && !strings.HasPrefix(strings.TrimSpace(block.Text), "<system-reminder>") {
				parts = append(parts, block.Text)
			}
		}
		text = strings.Join(parts, " ")
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > 180 {
		text = string(runes[:180]) + "…"
	}
	if row.Type == "user" {
		return "你：" + text
	}
	return "Claude：" + text
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *sessionRouter) adminAddAccount(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, APIKey string }
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Name, in.APIKey = strings.TrimSpace(in.Name), strings.TrimSpace(in.APIKey)
	if in.APIKey == "" {
		http.Error(w, "api_key is required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Name == "" {
		in.Name = fmt.Sprintf("账号 %d", len(s.accounts)+1)
	}
	defaultModel := ""
	if len(s.accounts) > 0 {
		defaultModel = s.accounts[0].Model
	}
	if defaultModel == "" {
		models := make([]string, 0, len(s.cfg.Models))
		for id := range s.cfg.Models {
			models = append(models, id)
		}
		sort.Strings(models)
		if len(models) > 0 {
			defaultModel = models[0]
		}
	}
	a := config.Account{ID: "key-" + digest(in.APIKey), Name: in.Name, APIKey: in.APIKey, Model: defaultModel}
	for _, existing := range s.accounts {
		if existing.ID == a.ID || existing.Name == a.Name {
			http.Error(w, "duplicate account name or key", http.StatusConflict)
			return
		}
	}
	next := append(append([]config.Account(nil), s.accounts...), a)
	if err := s.persistAccounts(next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.accounts = next
	s.addRuntimeAccount(a)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(adminAccount{a.ID, a.Name, maskKey(a.APIKey), a.Model, true})
}

func (s *sessionRouter) adminEditAccount(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Name    string
		APIKey  string
		Model   string
		Enabled *bool
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]config.Account(nil), s.accounts...)
	idx := -1
	for i := range next {
		if next[i].ID == id {
			idx = i
		}
	}
	if idx < 0 {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if strings.TrimSpace(in.Name) != "" {
		next[idx].Name = strings.TrimSpace(in.Name)
	}
	if strings.TrimSpace(in.APIKey) != "" {
		next[idx].APIKey = strings.TrimSpace(in.APIKey)
	}
	if strings.TrimSpace(in.Model) != "" {
		model := strings.TrimSpace(in.Model)
		if _, ok := s.cfg.Models[model]; !ok {
			http.Error(w, "unknown model", http.StatusBadRequest)
			return
		}
		next[idx].Model = model
	}
	if in.Enabled != nil {
		next[idx].Disabled = !*in.Enabled
	}
	for i, a := range next {
		if i != idx && a.Name == next[idx].Name {
			http.Error(w, "duplicate account name", http.StatusConflict)
			return
		}
	}
	enabled := 0
	for _, a := range next {
		if !a.Disabled {
			enabled++
		}
	}
	if enabled == 0 {
		http.Error(w, "at least one account must remain enabled", http.StatusConflict)
		return
	}
	if err := s.persistAccounts(next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.accounts = next
	if strings.TrimSpace(in.APIKey) != "" {
		for hash, b := range s.bindings {
			if b.Account == id {
				b.Fingerprint = digest(next[idx].APIKey)
				s.bindings[hash] = b
				if err := s.appendBinding(b); err != nil {
					s.poisoned = true
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
		s.addRuntimeAccount(next[idx])
	}
	_ = json.NewEncoder(w).Encode(adminAccount{id, accountName(next[idx], idx), maskKey(next[idx].APIKey), next[idx].Model, !next[idx].Disabled})
}

func (s *sessionRouter) adminDeleteAccount(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if b.Account == id {
			http.Error(w, "move this account's sessions before deleting it", http.StatusConflict)
			return
		}
	}
	next := make([]config.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		if a.ID != id {
			next = append(next, a)
		}
	}
	if len(next) == len(s.accounts) {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if len(next) == 0 {
		http.Error(w, "at least one account is required", http.StatusConflict)
		return
	}
	if err := s.persistAccounts(next); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.accounts = next
	delete(s.handlers, id)
	delete(s.breakers, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *sessionRouter) adminMoveSession(w http.ResponseWriter, r *http.Request, hash string) {
	var in struct {
		AccountID string `json:"account_id"`
		Model     string `json:"model"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[hash]
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if in.AccountID == "" && in.Model == "" {
		http.Error(w, "account_id or model is required", http.StatusBadRequest)
		return
	}
	if in.AccountID != "" {
		var target *config.Account
		for i := range s.accounts {
			if s.accounts[i].ID == in.AccountID {
				target = &s.accounts[i]
			}
		}
		if target == nil {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		b.Account, b.Fingerprint = target.ID, digest(target.APIKey)
	}
	if in.Model != "" {
		model := strings.TrimSpace(in.Model)
		if _, ok := s.cfg.Models[model]; !ok {
			http.Error(w, "unknown model", http.StatusBadRequest)
			return
		}
		b.Model = model
	}
	if err := s.appendBinding(b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.bindings[hash] = b
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *sessionRouter) appendBinding(b binding) error {
	data, _ := json.Marshal(b)
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *sessionRouter) persistAccounts(accounts []config.Account) error {
	if os.Getenv("OGC_API_KEYS") != "" {
		return fmt.Errorf("accounts are controlled by OGC_API_KEYS; unset it before editing")
	}
	data, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		return err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	raw["accounts"] = accounts
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.cfg.ConfigPath), ".ogc-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(append(out, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, s.cfg.ConfigPath); err == nil {
		return nil
	}
	// Windows cannot replace an existing file. Keep a recoverable backup while swapping.
	backup := s.cfg.ConfigPath + ".admin-backup"
	_ = os.Remove(backup)
	if err = os.Rename(s.cfg.ConfigPath, backup); err != nil {
		return err
	}
	if err = os.Rename(tmpName, s.cfg.ConfigPath); err != nil {
		_ = os.Rename(backup, s.cfg.ConfigPath)
		return err
	}
	_ = os.Remove(backup)
	return nil
}

var adminPage = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>OGC 账号与会话</title><style>
body{font:14px system-ui;margin:0;background:#f5f6f8;color:#18202a}.wrap{max-width:1100px;margin:32px auto;padding:0 18px}h1{font-size:24px}section{background:white;border:1px solid #ddd;border-radius:12px;padding:18px;margin:16px 0}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:10px;border-bottom:1px solid #eee}input,select,button{padding:8px;border:1px solid #bbb;border-radius:7px}button{cursor:pointer;background:#fff}.primary{background:#1769e0;color:white;border-color:#1769e0}.danger{color:#b00020}.muted{color:#68707b}.row{display:flex;gap:8px;flex-wrap:wrap}.grow{flex:1;min-width:180px}.account-name{display:flex;align-items:center;gap:10px}.account-name input[type=checkbox]{width:18px;height:18px;cursor:pointer}.account-name input[type=text]{min-width:150px}.icon-button{font-size:17px;line-height:1;padding:7px 9px}.session-preview{position:relative;display:inline-block;cursor:help}.session-preview[data-preview]:hover:after,.session-preview[data-preview]:focus:after{content:attr(data-preview);position:absolute;z-index:20;left:0;top:calc(100% + 8px);width:min(480px,75vw);padding:10px 12px;border-radius:8px;background:#18202a;color:#fff;white-space:pre-wrap;line-height:1.45;box-shadow:0 6px 24px #0004;pointer-events:none}#msg{position:fixed;right:20px;bottom:20px;background:#222;color:#fff;padding:10px 14px;border-radius:8px;display:none}@media(max-width:650px){.optional{display:none}}
</style></head><body><main class="wrap"><h1>OGC 账号与会话</h1><p class="muted">Key 只显示首尾字符。会话改绑从下一次请求生效。</p>
<section><h2>账号</h2><div class="row"><input id="key" class="grow" type="password" placeholder="输入新账号的 API key"><button class="primary" onclick="addAccount()">添加</button></div><table><thead><tr><th>启用与名称</th><th>Key</th><th>默认模型</th><th>操作</th></tr></thead><tbody id="accounts"></tbody></table></section>
<section><h2>会话绑定</h2><input id="filter" placeholder="搜索 session ID" oninput="renderSessions()"><table><thead><tr><th>Session ID</th><th>最后通讯时间</th><th>账号</th><th>真实模型</th></tr></thead><tbody id="sessions"></tbody></table></section></main><div id="msg"></div>
<script>
let state={accounts:[],sessions:[]};const esc=s=>String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
async function api(path,opt){let r=await fetch('/admin/api/'+path,{headers:{'Content-Type':'application/json'},...opt});if(!r.ok)throw Error(await r.text());return r.status===204?null:r.json()}
function toast(s){let e=document.querySelector('#msg');e.textContent=s.trim();e.style.display='block';setTimeout(()=>e.style.display='none',3500)}
async function load(){state=await api('state');renderAccounts();renderSessions()}
function renderAccounts(){accounts.innerHTML=state.accounts.map(a=>"<tr style=\"opacity:"+(a.enabled?'1':'.55')+"\"><td><div class=\"account-name\"><input type=\"checkbox\" title=\"启用或禁用此账号\" "+(a.enabled?'checked':'')+" onchange=\"editAccount('"+a.id+"','','','',this.checked)\"><input type=\"text\" title=\"可直接修改账号名称\" value=\""+esc(a.name)+"\" onchange=\"editAccount('"+a.id+"',this.value,'','',null)\"></div></td><td>"+esc(a.masked_key)+"</td><td><select onchange=\"editAccount('"+a.id+"','','',this.value,null)\">"+state.models.map(m=>"<option value=\""+esc(m)+"\" "+(m===a.model?'selected':'')+">"+esc(m)+"</option>").join('')+"</select></td><td><button class=\"danger icon-button\" title=\"删除账号\" aria-label=\"删除账号\" onclick=\"delAccount('"+a.id+"')\">🗑️</button></td></tr>").join('')}
function renderSessions(){let q=filter.value.toLowerCase();sessions.innerHTML=state.sessions.filter(s=>s.session_id.toLowerCase().includes(q)).map(s=>"<tr><td><span class=\"session-preview\" tabindex=\"0\" data-preview=\"悬停加载最近对话\" onmouseenter=\"loadSessionPreview('"+s.hash+"',this)\" onfocus=\"loadSessionPreview('"+s.hash+"',this)\"><code>"+esc(s.session_id)+"</code></span></td><td>"+(s.last_seen?new Date(s.last_seen).toLocaleString():'—')+"</td><td><select onchange=\"moveSession('"+s.hash+"',this.value)\">"+state.accounts.map(a=>"<option value=\""+a.id+"\" "+(a.id===s.account_id?'selected':'')+">"+esc(a.name)+"</option>").join('')+"</select></td><td><select onchange=\"changeSessionModel('"+s.hash+"',this.value)\">"+(s.model?'':"<option value=\"\" selected disabled>等待下次通讯</option>")+state.models.map(m=>"<option value=\""+esc(m)+"\" "+(m===s.model?'selected':'')+">"+esc(m)+"</option>").join('')+"</select></td></tr>").join('')}
async function loadSessionPreview(hash,el){if(el.dataset.loaded)return;el.dataset.loaded='1';el.dataset.preview='正在加载…';try{let r=await api('sessions/'+hash+'/preview');el.dataset.preview=r.preview||'没有可预览的对话文本'}catch(e){el.dataset.preview='预览加载失败';el.dataset.loaded=''}}
async function addAccount(){try{await api('accounts',{method:'POST',body:JSON.stringify({APIKey:key.value})});key.value='';await load();toast('已添加，可在列表中修改名称')}catch(e){toast(e.message)}}
async function editAccount(id,n,k,model,enabled){try{let body={Name:n,APIKey:k,Model:model};if(enabled!==null)body.Enabled=enabled;await api('accounts/'+id,{method:'PATCH',body:JSON.stringify(body)});await load();toast('已保存；默认模型只影响新 session')}catch(e){toast(e.message);await load()}}
async function delAccount(id){let a=state.accounts.find(x=>x.id===id),count=state.sessions.filter(s=>s.account_id===id).length;if(count){alert('账号「'+a.name+'」仍绑定 '+count+' 个 session，不能删除。请先在会话列表中把它们改绑到其他账号。');return}if(!confirm('确认删除账号「'+a.name+'」？\n\n该 key 将从配置中永久移除，此操作不可撤销。'))return;try{await api('accounts/'+id,{method:'DELETE'});await load();toast('已删除')}catch(e){toast(e.message)}}
async function moveSession(hash,id){try{await api('sessions/'+hash,{method:'PATCH',body:JSON.stringify({account_id:id})});await load();toast('会话已改绑')}catch(e){toast(e.message);await load()}}
async function changeSessionModel(hash,model){try{await api('sessions/'+hash,{method:'PATCH',body:JSON.stringify({model:model})});await load();toast('会话模型已切换；下次请求立即生效')}catch(e){toast(e.message);await load()}}
load().catch(e=>toast(e.message));
</script></body></html>`))
