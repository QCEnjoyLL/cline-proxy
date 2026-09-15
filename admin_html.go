package main

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Cline 代理管理面板</title>
<style>
:root{--bg:#0d1117;--bg2:#161b22;--bg3:#21262d;--border:#30363d;--text:#e6edf3;--text2:#8b949e;--accent:#58a6ff;--green:#3fb950;--red:#f85149;--yellow:#d29922;--blue:#58a6ff}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif;background:var(--bg);color:var(--text);font-size:14px;line-height:1.5}
.layout{display:flex;min-height:100vh}
.sidebar{width:240px;background:var(--bg2);border-right:1px solid var(--border);padding:16px 0;flex-shrink:0;display:flex;flex-direction:column}
.sidebar h1{font-size:16px;padding:0 16px 16px;border-bottom:1px solid var(--border);margin-bottom:8px;display:flex;align-items:center;gap:8px}
.sidebar h1 span{color:var(--accent)}
.nav-item{display:flex;align-items:center;gap:10px;padding:8px 16px;cursor:pointer;color:var(--text2);transition:0.15s;border-left:2px solid transparent}
.nav-item:hover{color:var(--text);background:var(--bg3)}
.nav-item.active{color:var(--text);background:var(--bg3);border-left-color:var(--accent)}
.main{flex:1;padding:24px 32px;overflow-y:auto}
h2{font-size:20px;margin-bottom:16px;font-weight:600}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px;margin-bottom:24px}
.card{background:var(--bg2);border:1px solid var(--border);border-radius:8px;padding:16px}
.card .num{font-size:28px;font-weight:600}
.card .label{font-size:12px;color:var(--text2);margin-top:4px}
.card .num.green{color:var(--green)}
.card .num.red{color:var(--red)}
.card .num.yellow{color:var(--yellow)}
.card .num.blue{color:var(--blue)}
.section{background:var(--bg2);border:1px solid var(--border);border-radius:8px;margin-bottom:24px;overflow:hidden}
.section-title{padding:12px 16px;border-bottom:1px solid var(--border);font-weight:600;display:flex;align-items:center;gap:8px}
.section-body{padding:16px}
.tabs{display:flex;border-bottom:1px solid var(--border)}
.tab{padding:10px 20px;cursor:pointer;color:var(--text2);border-bottom:2px solid transparent;font-size:13px}
.tab:hover{color:var(--text)}
.tab.active{color:var(--text);border-bottom-color:var(--accent)}
.tab-content{display:none;padding:16px}
.tab-content.active{display:block}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:8px 12px;border-bottom:1px solid var(--border);font-size:13px}
th{color:var(--text2);font-weight:600;font-size:12px}
.status{display:inline-flex;align-items:center;gap:4px;padding:2px 8px;border-radius:12px;font-size:12px;font-weight:500}
.status.active{background:#0e4429;color:var(--green)}
.status.cooldown{background:#3d2e00;color:var(--yellow)}
.status.expired{background:#3d1117;color:var(--red)}
.status-dot{width:6px;height:6px;border-radius:50%;display:inline-block}
.status-dot.active{background:var(--green)}
.status-dot.cooldown{background:var(--yellow)}
.status-dot.expired{background:var(--red)}
.btn{display:inline-flex;align-items:center;gap:6px;padding:6px 14px;border:1px solid var(--border);border-radius:6px;background:var(--bg3);color:var(--text);cursor:pointer;font-size:13px;transition:0.15s;text-decoration:none}
.btn:hover{background:var(--border)}
.btn-primary{background:#1f6feb;border-color:#1f6feb;color:#fff}
.btn-primary:hover{background:#388bfd}
.btn-success{background:#1a7f37;border-color:#1a7f37;color:#fff}
.btn-success:hover{background:#238636}
.btn-danger{border-color:var(--red);color:var(--red)}
.btn-danger:hover{background:#3d1117}
.btn-sm{padding:3px 10px;font-size:12px}
input,textarea,select{width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:13px;font-family:inherit}
input:focus,textarea:focus{outline:none;border-color:var(--accent)}
textarea{resize:vertical;min-height:80px;font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12px}
.form-row{display:flex;gap:12px;align-items:flex-end;margin-bottom:12px}
.form-row .field{flex:1}
.form-row .field label{display:block;font-size:12px;color:var(--text2);margin-bottom:4px}
.form-actions{display:flex;gap:8px;margin-top:12px}
.toast{position:fixed;top:20px;right:20px;padding:12px 20px;border-radius:8px;color:#fff;z-index:9999;opacity:0;transform:translateY(-10px);transition:0.3s;font-size:13px;max-width:400px}
.toast.show{opacity:1;transform:translateY(0)}
.toast.success{background:#1a7f37}
.toast.error{background:var(--red)}
.toast.info{background:#1f6feb}
.loading{display:inline-block;width:14px;height:14px;border:2px solid var(--text2);border-top-color:var(--accent);border-radius:50%;animation:spin 0.8s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}
.empty{padding:32px;text-align:center;color:var(--text2)}
.mono{font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12px}
.flex{display:flex;align-items:center;gap:8px}
.gap-4{gap:4px}
.text-right{text-align:right}
.mt-8{margin-top:8px}
.inline-flex{display:inline-flex;align-items:center;gap:6px}
.key-display{background:var(--bg);padding:8px 12px;border-radius:6px;border:1px solid var(--border);font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12px;word-break:break-all;cursor:pointer}
.key-display:hover{background:var(--bg3)}
.copy-icon{cursor:pointer;color:var(--text2);padding:2px 6px;border-radius:4px}
.copy-icon:hover{color:var(--text);background:var(--bg3)}
.empty-state{padding:48px;text-align:center;color:var(--text2)}
.empty-state .icon{font-size:48px;margin-bottom:12px;display:block}
.model-tag{display:inline-block;padding:3px 8px;border-radius:4px;font-size:11px;background:var(--bg3);color:var(--text2);margin:2px}
.model-tag.free{border:1px solid var(--green);color:var(--green)}
.model-tag.pass{border:1px solid var(--yellow);color:var(--yellow)}
.model-tag.custom{border:1px solid var(--accent);color:var(--accent)}
.model-tag button{border:0;background:none;color:inherit;cursor:pointer;padding:0 0 0 6px;font-size:12px}
.justify-between{display:flex;justify-content:space-between;align-items:center}
/* ===== 模型库 ===== */
.mgroup{margin-bottom:26px}
.mgroup-head{display:flex;align-items:center;gap:9px;flex-wrap:wrap;padding-bottom:9px;margin-bottom:14px;border-bottom:1px solid var(--border)}
.mgroup-head .dot{width:8px;height:8px;border-radius:50%;flex:none}
.mgroup-head h3{font-size:15px;font-weight:600;margin:0}
.mgroup-head .gsub{font-size:12px;color:var(--text2)}
.mgroup-head .gcount{font-size:12px;color:var(--text2);margin-left:auto;white-space:nowrap}
.mcards{display:flex;flex-wrap:wrap;gap:8px}
.mcard{display:flex;flex-direction:column;gap:2px;max-width:100%;padding:7px 11px;background:var(--bg3);border:1px solid var(--border);border-radius:6px;cursor:pointer;transition:background .15s,border-color .15s}
.mcard:hover{border-color:var(--accent)}
.mcard:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.mcard .mrow{display:flex;align-items:center;gap:7px;min-width:0}
.mcard .mname{font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12.5px;font-weight:600;color:var(--accent);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.mcard .mid{font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:11px;color:var(--text2);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.mcard .mdesc{display:none;font-size:11.5px;color:var(--text2);line-height:1.45;max-width:52ch;margin-top:2px}
body.show-mdesc .mcard .mdesc{display:block}
.mcard .mini{border:0;background:none;color:var(--text2);cursor:pointer;padding:1px 4px;border-radius:4px;font-size:11px;line-height:1.4}
.mcard .mini:hover{color:var(--text);background:var(--bg)}
.mcard .mini.add:hover{color:var(--green)}
.mcard.installed{border-color:var(--green);background:rgba(63,185,80,.07)}
.mcard.installed .mname{color:var(--green)}
.mcard.installed .mini.add{display:none}
.badge{flex:none;font-size:9.5px;font-weight:700;letter-spacing:.05em;text-transform:uppercase;padding:1px 5px;border-radius:4px;background:#3d2e00;color:var(--yellow)}
.badge.ok{background:#0e4429;color:var(--green)}
</style>
</head>
<body>
<div class="layout">
<div class="sidebar">
<h1><span>⚡</span>Cline 代理</h1>
<div class="nav-item active" data-tab="dashboard"><span>📊</span> 仪表盘</div>
<div class="nav-item" data-tab="models"><span>🧠</span> 模型库</div>
<div class="nav-item" data-tab="accounts"><span>👤</span> 账号管理</div>
<div class="nav-item" data-tab="import"><span>📥</span> 导入账号</div>
<div class="nav-item" data-tab="settings"><span>⚙️</span> 设置</div>
<div style="margin-top:auto;padding:16px;font-size:12px;color:var(--text2)">
  <div>管理面板: <a href="/admin/" style="color:var(--accent)">/admin/</a></div>
  <div>API 地址: <span id="footerApiAddr">http://127.0.0.1:3457</span></div>
  <div style="margin-top:12px"><a class="btn btn-sm" href="/admin/logout">退出登录</a></div>
</div>
</div>

<div class="main">

<div id="tab-dashboard" class="tab-panel">
<h2>📊 仪表盘</h2>
<div class="cards">
  <div class="card"><div class="num blue" id="statTotal">-</div><div class="label">账号总数</div></div>
  <div class="card"><div class="num green" id="statActive">-</div><div class="label">活跃</div></div>
  <div class="card"><div class="num yellow" id="statCooldown">-</div><div class="label">冷却</div></div>
  <div class="card"><div class="num red" id="statExpired">-</div><div class="label">已过期</div></div>
</div>
<div class="section">
  <div class="section-title">📋 快捷操作</div>
  <div class="section-body" style="display:flex;gap:8px;flex-wrap:wrap">
    <button class="btn btn-primary" onclick="switchTab('import')">➕ 添加账号</button>
    <button class="btn" onclick="refreshAllTokens()">🔄 刷新全部 Token</button>
    <button class="btn" onclick="document.getElementById('fileInput').click()">📄 从文件导入</button>
    <input type="file" id="fileInput" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    <button class="btn" onclick="switchTab('settings');generateKey()">🔑 生成 API 密钥</button>
  </div>
</div>
</div>
<div id="tab-models" class="tab-panel" style="display:none">
<div class="flex justify-between" style="margin-bottom:16px">
  <h2>🧠 模型库</h2>
  <div style="display:flex;gap:8px;flex-wrap:wrap">
    <span id="mStatus" style="font-size:12px;color:var(--text2);align-self:center"></span>
    <button class="btn btn-sm" id="mDescBtn" onclick="toggleModelDesc()">显示描述</button>
    <button class="btn btn-sm" id="mRefreshBtn" onclick="loadRecommended(true)">🔄 刷新数据</button>
  </div>
</div>

<div class="section">
  <div class="section-title">🧩 可用模型分组</div>
  <div class="section-body">
    <p style="color:var(--text2);margin-bottom:12px">
      数据来自 <span class="mono">api.cline.bot</span>（由代理服务端抓取，浏览器直连会被 CORS 拦截）。
      点击分组标题右侧的「全部添加」可一键加入该组所有模型，点击单个模型卡片可单独添加，重复模型不会重复添加。
    </p>
    <div id="recommendedGroups">加载中...</div>
  </div>
</div>

<div class="section">
  <div class="section-title">✅ 已启用的模型</div>
  <div class="section-body">
    <p style="color:var(--text2);margin-bottom:12px">默认模型始终保留；自定义模型会出现在 <span class="mono">/v1/models</span> 中。点击标签上的 ✕ 可移除。</p>
    <div id="modelsList">加载中...</div>
  </div>
</div>
</div>

<div id="tab-accounts" class="tab-panel" style="display:none">
<div class="flex justify-between" style="margin-bottom:16px">
  <h2>👤 账号管理</h2>
  <div style="display:flex;gap:8px">
    <button class="btn btn-primary btn-sm" onclick="switchTab('import')">➕ 添加</button>
    <button class="btn btn-sm" onclick="exportAccounts()">📤 批量导出</button>
    <button class="btn btn-sm" onclick="loadAccounts()">🔄 刷新</button>
  </div>
</div>
<div class="section">
  <div class="section-body" style="padding:0">
    <table>
      <thead>
        <tr><th>邮箱</th><th>状态</th><th>今日使用</th><th>总使用次数</th><th>最后使用</th><th>创建时间</th><th>操作</th></tr>
      </thead>
      <tbody id="accountTableBody">
        <tr><td colspan="7" class="empty">加载中...</td></tr>
      </tbody>
    </table>
  </div>
</div>
</div>

<div id="tab-import" class="tab-panel" style="display:none">
<h2>📥 导入账号</h2>
<div class="section">
  <div class="tabs" id="importTabs">
    <div class="tab active" data-tab="oauth">🔑 OAuth 浏览器登录</div>
    <div class="tab" data-tab="token">✏️ 手动输入 Token</div>
    <div class="tab" data-tab="batch">📦 批量导入</div>
  </div>

  <div id="import-oauth" class="tab-content active">
    <p style="color:var(--text2);margin-bottom:12px">通过浏览器完成 OAuth 认证，支持 Google/GitHub/邮箱登录，自动获取 refreshToken。</p>
    <button class="btn btn-primary" onclick="startOAuth()" id="oauthBtn">🚀 开始 OAuth 登录</button>
    <div id="oauthProgress" style="display:none;margin-top:12px">
      <div style="display:flex;align-items:center;gap:12px">
        <div class="loading"></div>
        <div>
          <div style="font-weight:500" id="oauthStatus">等待浏览器授权...</div>
          <div style="color:var(--text2);font-size:12px;margin-top:4px">
            打开 <a href="#" id="oauthUrl" target="_blank" style="color:var(--accent)"></a>
            并输入代码: <strong id="oauthUserCode"></strong>
          </div>
        </div>
      </div>
    </div>
    <div id="oauthResult" style="display:none;margin-top:12px"></div>
  </div>

  <div id="import-token" class="tab-content">
    <p style="color:var(--text2);margin-bottom:12px">输入已有的 Cline refreshToken，系统会自动验证并加入池。</p>
    <div class="form-row">
      <div class="field">
        <label>Refresh Token *</label>
        <input type="text" id="tokenInput" placeholder="粘贴 refreshToken">
      </div>
    </div>
    <div class="form-row">
      <div class="field">
        <label>邮箱（可选，留空自动生成）</label>
        <input type="text" id="tokenEmail" placeholder="user@example.com">
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="addByToken()">➕ 添加账号</button>
    </div>
    <div id="tokenResult" style="margin-top:8px"></div>
  </div>

  <div id="import-batch" class="tab-content">
    <p style="color:var(--text2);margin-bottom:12px">批量导入多个账号。支持 JSON 数组或每行一个 token。</p>
    <div class="form-row">
      <div class="field">
        <label>JSON 数组格式：[{"refreshToken":"...","email":"..."}]</label>
        <textarea id="batchInput" placeholder='[{"refreshToken":"xxx","email":"u1@x.com"},{"refreshToken":"yyy","email":"u2@x.com"}]'></textarea>
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="batchImport()">📦 导入全部</button>
      <button class="btn" onclick="document.getElementById('fileInput2').click()">📄 选择文件</button>
      <input type="file" id="fileInput2" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    </div>
    <div id="batchResult" style="margin-top:8px"></div>
  </div>
</div>
</div>

<div id="tab-settings" class="tab-panel" style="display:none">
<h2>⚙️ 设置</h2>

<div class="section">
  <div class="section-title">🔑 API 密钥管理</div>
  <div class="section-body">
    <p style="color:var(--text2);margin-bottom:12px">生成的密钥可用于客户端访问代理 API（作为 x-api-key 或 Authorization 头）。</p>
    <div class="form-actions" style="margin-bottom:12px">
      <button class="btn btn-success" onclick="generateKey()">➕ 生成新密钥</button>
    </div>
    <div id="keysList"></div>
    <div id="keyGenResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🧠 手工添加模型</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field">
        <label>自定义模型 ID</label>
        <input type="text" id="customModelId" maxlength="200" placeholder="openai/gpt-4.1-nano">
      </div>
      <button class="btn btn-primary" onclick="addCustomModel()">➕ 添加模型</button>
    </div>
    <p style="color:var(--text2);margin-bottom:10px">
      默认模型始终保留；自定义模型会出现在 <span class="mono">/v1/models</span> 中。
      想从 Cline 官方清单里挑选，请到 <a href="#" onclick="switchTab('models');return false;" style="color:var(--accent)"> 模型库</a>。
    </p>
    <div class="models-tags" id="settingsModelsList">加载中...</div>
  </div>
</div>

<div class="section">
  <div class="section-title">🔧 代理配置</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>监听地址</label><input type="text" id="settingAddr" disabled></div>
      <div class="field"><label>默认模型</label><select id="settingDefModel" onchange="updateDefaultModel()"></select></div>
    </div>
    <div class="form-row">
      <div class="field">
        <label>轮询策略</label>
        <select id="settingStrategy" onchange="updateConfig()">
          <option value="round_robin">轮询 (round_robin)</option>
          <option value="fill">填满 (fill)</option>
          <option value="random">随机 (random)</option>
        </select>
      </div>
      <div class="field"><label>引擎版本</label><input type="text" id="settingVersion" disabled></div>
    </div>
    <div class="form-row">
      <div class="field"><label>账号文件</label><input type="text" id="settingPoolPath" disabled></div>
    </div>
  </div>
</div>

<div class="section">
  <div class="section-title">📨 请求头配置（模拟 Cline CLI 发出）</div>
  <div class="section-body">
    <table>
      <thead><tr><th style="width:220px">请求头</th><th>值</th><th style="width:40px"></th></tr></thead>
      <tbody id="headersTableBody">
        <tr><td colspan="3" class="empty">加载中...</td></tr>
      </tbody>
    </table>
    <div class="form-actions" style="margin-top:12px">
      <button class="btn btn-sm" onclick="addHeaderRow()">➕ 添加请求头</button>
      <button class="btn btn-sm btn-primary" onclick="saveHeaders()">💾 保存请求头</button>
    </div>
    <div style="font-size:12px;color:var(--text2);margin-top:8px">这些请求头会附加到所有转发给 Cline API 的请求中，以模拟官方客户端行为。</div>
    <div id="headerSaveResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🗑️ 危险操作</div>
  <div class="section-body">
    <div style="display:flex;gap:8px;flex-wrap:wrap">
      <button class="btn btn-danger" onclick="deleteAllAccounts()">🗑️ 删除全部账号</button>
      <button class="btn btn-danger" onclick="deleteAllKeys()">🗑️ 删除全部密钥</button>
    </div>
  </div>
</div>
</div>

</div>
</div>

<div id="toast" class="toast"></div>

<script>
const API = '/admin/api';

const _ = id => document.getElementById(id);
const esc = s => { const d=document.createElement('div'); d.textContent=s||''; return d.innerHTML; };

function toast(msg, t) {
  const el = _('toast');
  el.textContent = msg;
  el.className = 'toast ' + t + ' show';
  setTimeout(() => el.classList.remove('show'), 3500);
}

// ========== 导航 ==========
document.querySelectorAll('.nav-item').forEach(el => {
  el.addEventListener('click', () => {
    if (el.classList.contains('active')) return;
    document.querySelectorAll('.nav-item').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
    _('tab-' + el.dataset.tab).style.display = 'block';
    if (el.dataset.tab === 'dashboard') { loadStats(); loadAccounts(); }
    if (el.dataset.tab === 'accounts') loadAccounts();
    if (el.dataset.tab === 'models') { loadRecommended(false); loadModels(); }
    if (el.dataset.tab === 'settings') { loadKeys(); loadModels(); loadConfig(); }
  });
});

function switchTab(name) {
  document.querySelectorAll('.nav-item').forEach(e => {
    e.classList.toggle('active', e.dataset.tab === name);
  });
  document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
  _('tab-' + name).style.display = 'block';
  if (name === 'dashboard') { loadStats(); loadAccounts(); }
  if (name === 'accounts') loadAccounts();
  if (name === 'models') { loadRecommended(false); loadModels(); }
  if (name === 'settings') { loadKeys(); loadModels(); }
}

// 导入子标签
document.querySelectorAll('#importTabs .tab').forEach(el => {
  el.addEventListener('click', () => {
    document.querySelectorAll('#importTabs .tab').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('#import-oauth,#import-token,#import-batch').forEach(e => e.classList.remove('active'));
    _('import-' + el.dataset.tab).classList.add('active');
  });
});

// ========== API 请求 ==========
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const res = await fetch(API + path, opts);
  if (res.status === 401) { window.location.href = '/admin/login'; throw new Error('未登录'); }
  const data = await res.json();
  if (!data.success && data.error) throw new Error(data.error);
  return data;
}

// ========== 仪表盘 ==========
async function loadStats() {
  try {
    const d = await api('GET', '/stats');
    const s = d.data;
    _('statTotal').textContent = s.total;
    _('statActive').textContent = s.active;
    _('statCooldown').textContent = s.cooldown;
    _('statExpired').textContent = s.expired;
    if (s.version) _('settingVersion').value = s.version;
    if (s.strategy) _('settingStrategy').value = s.strategy;
  } catch (e) { /* ignore */ }
}

// ========== 账号管理 ==========
async function loadAccounts() {
  try {
    const d = await api('GET', '/accounts');
    const list = d.data.accounts;
    const tbody = _('accountTableBody');
    if (!list || list.length === 0) {
      tbody.innerHTML = '<tr><td colspan="7" class="empty">暂无账号，前往 <a href="#" onclick="switchTab(\'import\')" style="color:var(--accent);cursor:pointer">导入账号</a> 页添加</td></tr>';
      return;
    }
    const sn = { active: '活跃', cooldown: '冷却', expired: '已过期' };
    tbody.innerHTML = list.map(a => {
      const lu = a.lastUsed ? new Date(a.lastUsed).toLocaleString('zh-CN') : '-';
      const cr = a.createdAt ? new Date(a.createdAt).toLocaleString('zh-CN') : '-';
      return '<tr>' +
        '<td>' + esc(a.email) + '</td>' +
        '<td><span class="status ' + a.status + '"><span class="status-dot ' + a.status + '"></span>' + (sn[a.status] || a.status) + '</span></td>' +
        '<td>' + (a.dailyUsageCount || 0) + '</td>' +
        '<td>' + (a.usageCount || 0) + '</td>' +
        '<td class="mono" style="font-size:11px">' + lu + '</td>' +
        '<td class="mono" style="font-size:11px">' + cr + '</td>' +
        '<td style="white-space:nowrap">' +
          '<button class="btn btn-sm" onclick="resetAccount(\'' + a.accountId + '\')" title="重置">↻</button> ' +
          '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.accountId + '\')" title="删除">✕</button>' +
        '</td></tr>';
    }).join('');
  } catch (e) { toast('加载账号失败: ' + e.message, 'error'); }
}

async function exportAccounts() {
  try {
    const res = await fetch(API + '/accounts/export');
    if (res.status === 401) {
      window.location.href = '/admin/login';
      return;
    }
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = 'cline-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    toast('已导出全部账号', 'success');
  } catch (e) { toast('导出失败: ' + e.message, 'error'); }
}

async function deleteAccount(id) {
  if (!confirm('确定删除此账号？')) return;
  try {
    await api('POST', '/accounts/delete', { accountId: id });
    toast('账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function resetAccount(id) {
  if (!confirm('确定重置此账号？将清除今日使用计数和总使用计数，并刷新 Token。')) return;
  try {
    await api('POST', '/accounts/reset', { accountId: id });
    toast('账号已重置', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('重置失败: ' + e.message, 'error'); }
}

async function deleteAllAccounts() {
  if (!confirm('⚠️ 确定删除所有账号？不可撤销！')) return;
  try {
    await api('POST', '/accounts/delete-all', {});
    toast('全部账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function refreshAllTokens() {
  try {
    await api('POST', '/accounts/refresh-all', {});
    toast('全部 Token 已刷新', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

// ========== OAuth 登录 ==========
async function startOAuth() {
  const btn = _('oauthBtn');
  btn.disabled = true;
  btn.innerHTML = '<span class="loading"></span> 启动中...';
  _('oauthProgress').style.display = 'block';
  _('oauthResult').style.display = 'none';
  _('oauthStatus').textContent = '正在连接 WorkOS...';
  try {
    const d = await api('POST', '/oauth/start');
    const s = d.data;
    _('oauthStatus').textContent = '请在浏览器中打开链接并输入代码';
    const u = _('oauthUrl');
    u.textContent = s.verificationUri;
    u.href = s.verificationUri;
    _('oauthUserCode').textContent = s.userCode;
    const poll = setInterval(async () => {
      try {
        const r = await api('GET', '/oauth/status?sessionId=' + s.sessionId);
        if (r.data.done) {
          clearInterval(poll);
          btn.disabled = false;
          btn.innerHTML = '🚀 开始 OAuth 登录';
          if (r.data.success) {
            _('oauthProgress').style.display = 'none';
            _('oauthResult').innerHTML = '<div style="color:var(--green);font-weight:500">✓ 账号添加成功: ' + esc(r.data.email) + '</div>';
            _('oauthResult').style.display = 'block';
            loadAccounts(); loadStats();
            toast('账号添加成功！', 'success');
          } else {
            _('oauthStatus').textContent = '失败: ' + (r.data.error || '未知错误');
            toast('OAuth 失败', 'error');
          }
        }
      } catch(e) {}
    }, 2000);
  } catch (e) {
    btn.disabled = false;
    btn.innerHTML = '🚀 开始 OAuth 登录';
    _('oauthStatus').textContent = '错误: ' + e.message;
    toast('OAuth 失败: ' + e.message, 'error');
  }
}

// ========== Token 导入 ==========
async function addByToken() {
  const token = _('tokenInput').value.trim();
  if (!token) { toast('请输入 refreshToken', 'error'); return; }
  const email = _('tokenEmail').value.trim();
  try {
    const d = await api('POST', '/accounts/add', { refreshToken: token, email: email || undefined });
    toast('账号添加成功: ' + (d.data.email || ''), 'success');
    _('tokenInput').value = '';
    _('tokenEmail').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('添加失败: ' + e.message, 'error'); }
}

// ========== 批量导入 ==========
async function batchImport() {
  const raw = _('batchInput').value.trim();
  if (!raw) { toast('请输入账号数据', 'error'); return; }
  let tokens;
  try { tokens = JSON.parse(raw); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = raw.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入完成', 'success');
    _('batchInput').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
}

async function handleFileImport(event) {
  const file = event.target.files[0];
  if (!file) return;
  const text = await file.text();
  let tokens;
  try { tokens = JSON.parse(text); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = text.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入了 ' + tokens.length + ' 个账号', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
  event.target.value = '';
}

// ========== API 密钥管理 ==========
async function loadKeys() {
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys;
    const el = _('keysList');
    if (!keys || keys.length === 0) {
      el.innerHTML = '<div class="empty-state"><span class="icon">🔑</span>暂无 API 密钥</div>';
      return;
    }
    el.innerHTML = keys.map(k =>
      '<div class="flex" style="margin-bottom:8px">' +
        '<span class="key-display" style="flex:1" onclick="copyText(\'' + k + '\')" title="点击复制">' + esc(k) + '</span>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteKey(\'' + k + '\')">✕</button>' +
      '</div>'
    ).join('');
  } catch (e) { _('keysList').innerHTML = '<div class="empty">加载失败</div>'; }
}

async function generateKey() {
  try {
    const d = await api('POST', '/keys/generate');
    const key = d.data.key;
    _('keyGenResult').innerHTML =
      '<div style="background:var(--bg);border:1px solid var(--green);border-radius:6px;padding:12px">' +
        '<div style="color:var(--green);font-weight:500;margin-bottom:8px">✓ 新密钥已生成（点击复制）</div>' +
        '<div class="key-display" onclick="copyText(\'' + key + '\')">' + esc(key) + '</div>' +
      '</div>';
    loadKeys();
    toast('密钥已生成', 'success');
    setTimeout(() => _('keyGenResult').innerHTML = '', 8000);
  } catch (e) { toast('生成失败: ' + e.message, 'error'); }
}

async function deleteKey(key) {
  if (!confirm('确定删除此密钥？')) return;
  try {
    await api('POST', '/keys/delete', { key });
    toast('密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function deleteAllKeys() {
  if (!confirm('确定删除所有 API 密钥？')) return;
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys || [];
    for (const k of keys) await api('POST', '/keys/delete', { key: k });
    toast('全部密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

function copyText(t) {
  navigator.clipboard.writeText(t).then(() => toast('已复制到剪贴板', 'success')).catch(() => {
    const ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta);
    toast('已复制到剪贴板', 'success');
  });
}

// ========== 配置管理 ==========
async function updateConfig() {
  const strategy = _('settingStrategy').value;
  try {
    await api('POST', '/config/update', { strategy });
    toast('策略已更新为: ' + strategy, 'success');
  } catch (e) { toast('更新失败: ' + e.message, 'error'); }
}

async function updateDefaultModel() {
  const select = _('settingDefModel');
  const defaultModel = select.value;
  if (!defaultModel) return;
  select.disabled = true;
  try {
    const d = await api('POST', '/config/update', { defaultModel });
    select.dataset.selected = d.data.defaultModel;
    toast('默认模型已更新为: ' + d.data.defaultModel, 'success');
  } catch (e) {
    toast('默认模型更新失败: ' + e.message, 'error');
    await loadConfig();
  } finally { select.disabled = false; }
}

function addHeaderRow() {
  const tbody = _('headersTableBody');
  const tr = document.createElement('tr');
  tr.innerHTML =
    '<td><input type="text" class="header-key" placeholder="Header-Name" style="font-size:12px;font-family:monospace"></td>' +
    '<td><input type="text" class="header-val" placeholder="value" style="font-size:12px;font-family:monospace"></td>' +
    '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>';
  tbody.appendChild(tr);
}

async function saveHeaders() {
  const tbody = _('headersTableBody');
  const rows = tbody.querySelectorAll('tr');
  const headers = {};
  let hasEmpty = false;
  rows.forEach(tr => {
    const keyInput = tr.querySelector('.header-key');
    const valInput = tr.querySelector('.header-val');
    if (keyInput && valInput) {
      const k = keyInput.value.trim();
      const v = valInput.value.trim();
      if (k) { headers[k] = v; }
      else if (v) { hasEmpty = true; }
    }
  });
  if (hasEmpty) { toast('存在有值无键的行，已忽略', 'info'); }
  try {
    const d = await api('POST', '/config/update', { headers });
    toast('请求头已保存', 'success');
    _('headerSaveResult').innerHTML =
      '<div style="color:var(--green);font-size:12px">✓ 已保存 ' + Object.keys(d.data.headers).length + ' 个请求头</div>';
    setTimeout(() => _('headerSaveResult').innerHTML = '', 5000);
    loadConfig();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 模型列表 ==========
async function loadModels() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    const select = _('settingDefModel');
    const selectedModel = select.dataset.selected || select.value;
    select.replaceChildren();
    models.forEach(m => {
      const option = document.createElement('option');
      option.value = m.id;
      option.textContent = m.id;
      select.appendChild(option);
    });
    if (models.some(m => m.id === selectedModel)) select.value = selectedModel;
    const list = _('settingsModelsList');
    list.innerHTML = '';
    models.forEach(m => {
      const tag = document.createElement('span');
      tag.className = 'model-tag ' + (m.cost || 'free');
      tag.appendChild(document.createTextNode(m.id));
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.title = '\u5220\u9664\u6a21\u578b';
      remove.textContent = '\u2715';
      remove.addEventListener('click', () => deleteCustomModel(m.id));
      tag.appendChild(remove);
      list.appendChild(tag);
    });
    if (!models.length) list.innerHTML = '<div class="empty">\u6682\u65e0\u6a21\u578b</div>';
    // 同步模型库的「已启用」标记，避免两处状态不一致
    mInstalled = models.map(m => m.id);
    if (mDataGroups.length) renderRecommended();
  } catch (e) { _('settingsModelsList').textContent = '加载失败'; }
}

async function addCustomModel() {
  const input = _('customModelId');
  const id = input.value.trim();
  if (!id) { toast('\u8bf7\u8f93\u5165\u6a21\u578b ID', 'error'); return; }
  try {
    await api('POST', '/models', { id });
    input.value = '';
    await loadModels();
    toast('\u6a21\u578b\u5df2\u6dfb\u52a0', 'success');
  } catch (e) { toast('\u6dfb\u52a0\u5931\u8d25: ' + e.message, 'error'); }
}

async function deleteCustomModel(id) {
  try {
    await api('POST', '/models/delete', { id });
    await loadModels();
    await loadConfig();
    toast('\u6a21\u578b\u5df2\u5220\u9664', 'success');
  } catch (e) { toast('\u5220\u9664\u5931\u8d25: ' + e.message, 'error'); }
}

_('customModelId').addEventListener('keydown', e => {
  if (e.key === 'Enter') addCustomModel();
});

// ========== 模型库（推荐模型清单） ==========

// 分组显示名与主题色；未登记的分组按原始 key 展示。
const MGROUPS = {
  recommended: { title: '推荐模型', sub: '官方主推', color: '#d29922' },
  free: { title: 'free 模型', sub: '免费额度', color: '#3fb950' },
  clinePass: { title: 'Cline Pass', sub: '订阅套餐内', color: '#a371f7' },
  clineCloud: { title: 'Cline Cloud', sub: '云端托管', color: '#58a6ff' }
};

let mInstalled = [];      // 已启用（含默认）的模型 ID
let mDataGroups = [];     // 最近一次取到的分组数据
let mFetchedAt = 0;       // 数据抓取时间戳
let mAutoTimer = null;    // 自动刷新定时器

const AUTO_REFRESH_MS = 10 * 60 * 1000;   // 页面可见时每 10 分钟自动刷新一次

function relTime(ts) {
  if (!ts) return '未知';
  const diff = Date.now() - ts;
  if (diff < 45e3) return '刚刚';
  const m = Math.floor(diff / 60e3);
  if (m < 60) return m + ' 分钟前';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' 小时前';
  return Math.floor(h / 24) + ' 天前';
}

// force=true 时要求服务端回源刷新（忽略服务端缓存）
async function loadRecommended(force) {
  const btn = _('mRefreshBtn');
  const box = _('recommendedGroups');
  if (force) {
    btn.disabled = true;
    btn.textContent = '刷新中...';
    if (!mDataGroups.length) box.innerHTML = '<div class="empty">正在抓取…</div>';
  }
  try {
    const d = await api('GET', '/recommended-models' + (force ? '?refresh=1' : ''));
    mDataGroups = d.data.groups || [];
    mInstalled = d.data.installed || [];
    mFetchedAt = d.data.fetchedAt || 0;
    renderRecommended();
    if (force) toast('模型数据已刷新', 'success');
    if (d.data.stale && d.error) {
      toast('上游抓取失败，显示上次缓存: ' + d.error, 'error');
    }
    startAutoRefresh();
  } catch (e) {
    box.innerHTML = '<div class="empty">加载失败: ' + esc(e.message) + '</div>';
    _('mStatus').textContent = '';
    if (force) toast('刷新失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false;
    btn.textContent = '🔄 刷新数据';
  }
}

function renderRecommended() {
  const box = _('recommendedGroups');
  const groups = mDataGroups;
  if (!groups.length) {
    box.innerHTML = '<div class="empty">暂无数据，点击右上角「刷新数据」</div>';
    _('mStatus').textContent = '';
    return;
  }

  const installed = new Set(mInstalled);
  let total = 0;

  box.innerHTML = groups.map(g => {
    const meta = MGROUPS[g.key] || { title: g.key, sub: '', color: 'var(--text2)' };
    const models = g.models || [];
    total += models.length;
    // 该组中尚未添加的模型数量，用于决定「全部添加」是否可点
    const pending = models.filter(m => !installed.has(m.id)).length;

    const cards = models.map(m => {
      const has = installed.has(m.id);
      const tags = (m.tags || []).filter(Boolean).map(t =>
        '<span class="badge">' + esc(t) + '</span>').join('');
      const name = m.name || m.id;
      const tip = [name, m.id, m.description, '', has ? '已启用 · 点击复制 ID' : '点击添加'].filter(Boolean).join('\n');
      return '<div class="mcard' + (has ? ' installed' : '') + '" role="button" tabindex="0"' +
        ' data-id="' + esc(m.id) + '" data-installed="' + (has ? '1' : '0') + '"' +
        ' title="' + esc(tip) + '">' +
        '<div class="mrow">' +
          '<span class="mname">' + esc(name) + '</span>' +
          (m.id && m.id !== name ? '<span class="mid">' + esc(m.id) + '</span>' : '') +
          tags +
          '<span class="mini add" data-act="add" title="添加此模型">＋</span>' +
          '<span class="mini" data-act="copy" title="复制模型 ID">⧉</span>' +
        '</div>' +
        (m.description ? '<div class="mdesc">' + esc(m.description) + '</div>' : '') +
      '</div>';
    }).join('');

    return '<div class="mgroup" data-key="' + esc(g.key) + '">' +
      '<div class="mgroup-head">' +
        '<span class="dot" style="background:' + meta.color + '"></span>' +
        '<h3>' + esc(meta.title) + '</h3>' +
        (meta.sub ? '<span class="gsub">（' + esc(meta.sub) + '）</span>' : '') +
        '<span class="gcount">共 ' + models.length + ' 个' +
          (pending ? ' · 待添加 ' + pending : ' · 已全部添加') + '</span>' +
        '<button class="btn btn-sm' + (pending ? ' btn-success' : '') + '"' +
          (pending ? '' : ' disabled') +
          ' onclick="addGroup(\'' + g.key + '\')">' +
          (pending ? '➕ 全部添加 (' + pending + ')' : '✓ 已全部添加') + '</button>' +
      '</div>' +
      '<div class="mcards">' + cards + '</div>' +
    '</div>';
  }).join('');

  _('mStatus').textContent = '共 ' + total + ' 个 · 已启用 ' + mInstalled.length +
    ' · 数据 ' + relTime(mFetchedAt);
}

// 卡片点击：已启用的复制 ID，未启用的直接添加；点 ⧉ 始终复制。
async function onModelCardClick(e) {
  const card = e.target.closest('.mcard');
  if (!card) return;
  const id = card.dataset.id || '';
  const act = e.target.dataset.act;
  if (act === 'copy' || card.dataset.installed === '1') {
    await copyText(id);
    return;
  }
  await addModels([id], '模型已添加');
}

async function onModelCardKey(e) {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const card = e.target.closest('.mcard');
  if (!card) return;
  e.preventDefault();
  const id = card.dataset.id || '';
  if (card.dataset.installed === '1') { await copyText(id); return; }
  await addModels([id], '模型已添加');
}

// 一键添加一个分组的全部模型（已存在的由服务端跳过）
function addGroup(key) {
  const g = mDataGroups.find(x => x.key === key);
  if (!g) return;
  const installed = new Set(mInstalled);
  const ids = (g.models || []).map(m => m.id).filter(id => id && !installed.has(id));
  if (!ids.length) { toast('该分组模型已全部添加', 'info'); return; }
  const title = (MGROUPS[key] || {}).title || key;
  addModels(ids, title + ' 分组已添加');
}

// 统一走批量接口；服务端对已存在的 ID 返回 skipped，不会重复添加。
async function addModels(ids, okMsg) {
  if (!ids || !ids.length) return;
  try {
    const d = await api('POST', '/models/batch', { ids });
    const added = d.data.added || [];
    const skipped = d.data.skipped || [];
    const failed = d.data.failed || {};
    mInstalled = mInstalled.concat(added);
    renderRecommended();
    await loadModels();

    const parts = [];
    if (added.length) parts.push('新增 ' + added.length + ' 个');
    if (skipped.length) parts.push('已存在 ' + skipped.length + ' 个');
    const failCount = Object.keys(failed).length;
    if (failCount) parts.push('失败 ' + failCount + ' 个');

    if (failCount && !added.length) {
      toast('添加失败: ' + Object.values(failed)[0], 'error');
    } else if (!added.length) {
      toast('这些模型都已存在，未重复添加', 'info');
    } else {
      toast((okMsg || '已添加') + '（' + parts.join('，') + '）', 'success');
    }
  } catch (e) {
    toast('添加失败: ' + e.message, 'error');
  }
}

function toggleModelDesc() {
  const on = document.body.classList.toggle('show-mdesc');
  _('mDescBtn').textContent = on ? '隐藏描述' : '显示描述';
}

// 自动刷新：仅在「模型库」标签可见且页面处于前台时触发，避免无谓请求。
function startAutoRefresh() {
  if (mAutoTimer) return;
  mAutoTimer = setInterval(() => {
    const panel = _('tab-models');
    if (!panel || panel.style.display === 'none') return;
    if (document.hidden) return;
    loadRecommended(true);
  }, AUTO_REFRESH_MS);
}

_('recommendedGroups').addEventListener('click', onModelCardClick);
_('recommendedGroups').addEventListener('keydown', onModelCardKey);

// ========== 配置加载 ==========
async function loadConfig() {
  try {
    const d = await api('GET', '/config');
    const c = d.data;
    if (c.address) _('settingAddr').value = c.address;
    if (c.strategy) _('settingStrategy').value = c.strategy;
    if (c.version) _('settingVersion').value = c.version;
    if (c.poolPath) _('settingPoolPath').value = c.poolPath;
    if (c.defaultModel) {
      _('settingDefModel').dataset.selected = c.defaultModel;
      _('settingDefModel').value = c.defaultModel;
    }
    if (c.headers) {
      const tbody = _('headersTableBody');
      tbody.innerHTML = Object.entries(c.headers).map(([k, v]) =>
        '<tr>' +
          '<td><input type="text" class="header-key" value="' + esc(k) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><input type="text" class="header-val" value="' + esc(v) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>' +
        '</tr>'
      ).join('');
    }
  } catch (e) { /* ignore */ }
}

// ========== 初始化 ==========
loadStats();
loadAccounts();
loadKeys();
loadModels();
loadConfig();
// 预取模型库数据（服务端有 30 分钟缓存，不会每次都回源）
loadRecommended(false);
setInterval(() => { loadStats(); }, 10000);
</script>
</body>
</html>`

const adminLoginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Cline 代理 - 登录</title>
<style>
:root{--bg:#0d1117;--bg2:#161b22;--bg3:#21262d;--border:#30363d;--text:#e6edf3;--text2:#8b949e;--accent:#58a6ff;--red:#f85149}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif;background:radial-gradient(1200px 600px at 20% -10%,rgba(88,166,255,0.10),transparent 60%),radial-gradient(1000px 500px at 90% 110%,rgba(63,185,80,0.07),transparent 60%),var(--bg);color:var(--text);min-height:100vh;display:flex;align-items:center;justify-content:center;font-size:14px;line-height:1.5}
.login-wrap{width:100%;max-width:380px;padding:24px}
.card{background:var(--bg2);border:1px solid var(--border);border-radius:12px;padding:32px 28px;box-shadow:0 8px 40px rgba(0,0,0,0.45)}
.brand{display:flex;align-items:center;justify-content:center;gap:10px;margin-bottom:6px}
.brand .logo{width:40px;height:40px;border-radius:10px;background:linear-gradient(135deg,#1f6feb,#388bfd);display:flex;align-items:center;justify-content:center;font-size:20px;font-weight:700;color:#fff;box-shadow:0 4px 14px rgba(31,111,235,0.35)}
.brand h1{font-size:20px;font-weight:600}
.subtitle{text-align:center;color:var(--text2);font-size:12px;margin-bottom:26px}
.field{margin-bottom:14px}
.field label{display:block;font-size:12px;color:var(--text2);margin-bottom:6px}
input{width:100%;padding:10px 12px;background:var(--bg);border:1px solid var(--border);border-radius:8px;color:var(--text);font-size:14px;font-family:inherit;transition:border-color .15s,box-shadow .15s}
input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(88,166,255,0.15)}
button{width:100%;padding:11px;border:none;border-radius:8px;background:#1f6feb;color:#fff;font-size:14px;font-weight:600;cursor:pointer;transition:background .15s;margin-top:6px;font-family:inherit}
button:hover{background:#388bfd}
button:disabled{opacity:.6;cursor:not-allowed}
.error{display:none;margin-top:14px;padding:9px 12px;background:#3d1117;border:1px solid var(--red);color:#f85149;border-radius:8px;font-size:13px;text-align:center}
.error.show{display:block}
.hint{margin-top:18px;text-align:center;font-size:12px;color:var(--text2)}
.hint code{background:var(--bg3);padding:2px 6px;border-radius:4px;font-size:11px}
</style>
</head>
<body>
<div class="login-wrap">
  <div class="card">
    <div class="brand">
      <div class="logo">C</div>
      <h1>Cline 代理</h1>
    </div>
    <div class="subtitle">管理后台 · 请登录</div>
    <form id="loginForm" autocomplete="on">
      <div class="field">
        <label for="username">用户名</label>
        <input id="username" name="username" type="text" autocomplete="username" placeholder="admin" required autofocus>
      </div>
      <div class="field">
        <label for="password">密码</label>
        <input id="password" name="password" type="password" autocomplete="current-password" placeholder="请输入密码" required>
      </div>
      <button id="loginBtn" type="submit">登 录</button>
      <div class="error" id="error"></div>
    </form>
    <div class="hint">默认账号 <code>admin</code> · 密码未设置时见启动日志</div>
  </div>
</div>
<script>
(function(){
  var form=document.getElementById('loginForm'),btn=document.getElementById('loginBtn'),err=document.getElementById('error');
  form.addEventListener('submit',function(e){
    e.preventDefault();
    err.className='error';
    btn.disabled=true;
    btn.textContent='登录中...';
    fetch('/admin/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:document.getElementById('username').value,password:document.getElementById('password').value})})
      .then(function(res){ return res.json().then(function(d){ return {ok:res.ok,data:d}; }); })
      .then(function(r){
        if(r.ok && r.data.success){ window.location.href='/admin/'; return; }
        err.textContent=(r.data && r.data.error)||'登录失败，请重试';
        err.className='error show';
        btn.disabled=false;
        btn.textContent='登 录';
      })
      .catch(function(){ err.textContent='网络错误，请重试'; err.className='error show'; btn.disabled=false; btn.textContent='登 录'; });
  });
})();
</script>
</body>
</html>`
