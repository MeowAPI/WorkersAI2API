'use strict';

(() => {
  const $ = (id) => document.getElementById(id);
  const number = new Intl.NumberFormat('zh-CN');
  let state = null;
  let csrfToken = '';
  let currentTab = 'overview';
  let models = null;
  let modelsLoadingGeneration = null;
  let editingID = null;
  let deletingID = null;
  let toastTimer;
  let stateLoadingGeneration = null;
  let authGeneration = 0;
  let authenticated = false;

  function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function notify(message, error = false) {
    const toast = $('toast');
    clearTimeout(toastTimer);
    toast.textContent = message;
    toast.className = error ? 'toast error' : 'toast';
    toast.hidden = false;
    toastTimer = setTimeout(() => { toast.hidden = true; }, error ? 7000 : 4000);
  }

  function formError(id, message = '') {
    $(id).textContent = message;
    $(id).hidden = !message;
  }

  function showLogin(message = '') {
    authGeneration++;
    authenticated = false;
    csrfToken = '';
    state = null;
    models = null;
    selectedLog = null; detailVersion++; observationVersion.stats++; observationVersion.logs++;
    observationLoaded.stats = false; observationLoaded.logs = false;
    knownModels.clear(); knownAccounts.clear();
    $('logs-rows').replaceChildren(); $('stats-content').hidden = true;
    clearTimeout(searchTimer);
    for (const dialog of document.querySelectorAll('dialog[open]')) dialog.close();
    $('account-token').value = '';
    $('app-view').hidden = true;
    $('login-view').hidden = false;
    formError('login-error', message);
  }

  function showApp() {
    const wasAuthenticated = authenticated;
    authenticated = true;
    $('auth-token').value = '';
    $('login-view').hidden = true;
    $('app-view').hidden = false;
    $('api-base').textContent = `${location.origin}/v1`;
    if (!wasAuthenticated) { loadModels(); refreshActiveObservation(); }
  }

  async function api(path, options = {}) {
    const generation = authGeneration;
    const { method = 'GET', body, loginToken } = options;
    const headers = { Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (loginToken !== undefined) headers.Authorization = `Bearer ${loginToken}`;
    else if (method !== 'GET') headers['X-CSRF-Token'] = csrfToken;
    const response = await fetch(`/admin/api${path}`, {
      method, headers, credentials: 'same-origin', cache: 'no-store',
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    let data = null;
    if (response.status !== 204) {
      try { data = await response.json(); } catch (_) { /* Normalize non-JSON failures below. */ }
    }
    // A response from an earlier login session must never restore or alter it.
    if (generation !== authGeneration) {
      const error = new Error('会话已切换');
      error.status = 401;
      error.stale = true;
      throw error;
    }
    if (!response.ok) {
      const message = data?.error?.message || `请求失败（HTTP ${response.status}）`;
      if (response.status === 401 && loginToken === undefined) showLogin(authenticated ? '登录已失效，请重新输入管理密钥。' : '');
      const error = new Error(message);
      error.status = response.status;
      throw error;
    }
    return data;
  }

  function timestamp(value) {
    if (!value) return null;
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? null : date;
  }

  function formatDate(value) {
    const date = timestamp(value);
    return date ? date.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }) : '尚无请求';
  }

  function isCooling(account) {
    const until = timestamp(account.cooldown_until);
    return Boolean(until && until.getTime() > Date.now());
  }

  function availableAccounts(accounts) {
    return accounts.filter((account) => account.enabled && !isCooling(account));
  }

  function renderState() {
    if (!state) return;
    const accounts = state.accounts || [];
    const enabled = accounts.filter((account) => account.enabled).length;
    const available = availableAccounts(accounts);
    const requests = accounts.reduce((sum, account) => sum + (account.requests || 0), 0);
    const failures = accounts.reduce((sum, account) => sum + (account.failures || 0), 0);
    $('metric-total').textContent = number.format(accounts.length);
    $('metric-available').replaceChildren(document.createTextNode(number.format(available.length)), element('span', '', `/ ${enabled} 已启用`));
    $('metric-requests').textContent = number.format(requests);
    $('metric-failures').textContent = number.format(failures);
    $('nav-account-count').textContent = accounts.length;
    $('account-list-count').textContent = `/ ${String(accounts.length).padStart(2, '0')}`;
    $('nav-model-count').textContent = state.catalog?.count ?? '—';
    $('no-available').hidden = available.length > 0;
    $('service-dot').className = available.length ? 'status-dot' : 'status-dot warning';
    $('service-label').textContent = available.length ? '服务已连接' : '等待可用账号';
    $('account-empty').hidden = accounts.length > 0;
    $('account-rows').closest('.table-wrap').hidden = accounts.length === 0;
    renderAccounts(accounts);
    renderSequence(available);
    renderCatalogStatus();
    updateObservationFilters();
  }

  function renderSequence(available) {
    const sequence = $('routing-sequence');
    sequence.replaceChildren();
    if (!available.length) {
      sequence.append(element('span', 'section-note', '暂无可参与轮询的账号'));
      return;
    }
    const nextIndex = available.findIndex((account) => account.next);
    const ordered = nextIndex > 0 ? [...available.slice(nextIndex), ...available.slice(0, nextIndex)] : available;
    ordered.slice(0, 5).forEach((account, index) => {
      if (index) sequence.append(element('span', 'sequence-arrow', '→'));
      const chip = element('span', `sequence-account${account.next ? ' next' : ''}`);
      chip.title = account.name;
      if (account.next) chip.append(element('span', 'sequence-next', 'NEXT'));
      chip.append(document.createTextNode(account.name));
      sequence.append(chip);
    });
    if (ordered.length > 5) sequence.append(element('span', 'sequence-arrow', `+${ordered.length - 5}`));
    sequence.append(element('span', 'sequence-arrow', '↺'));
  }

  function renderAccounts(accounts) {
    const rows = document.createDocumentFragment();
    for (const account of accounts) {
      const row = element('tr');
      const identity = element('td');
      const title = element('div', 'account-title');
      const name = element('span', 'account-name', account.name);
      name.title = account.name;
      title.append(name);
      if (account.next) title.append(element('span', 'badge next-badge', 'NEXT'));
      identity.append(title, element('div', 'account-id', account.account_id));
      identity.append(element('div', 'account-token', `TOKEN ${account.token_hint || '••••••••'}`));
      const statusCell = element('td');
      statusCell.dataset.label = '状态';
      const cooling = isCooling(account);
      const status = !account.enabled ? '已停用' : cooling ? '冷却中' : '可用';
      statusCell.append(element('span', `badge${!account.enabled ? ' disabled' : cooling ? ' cooling' : ''}`, status));
      if (account.enabled && cooling) statusCell.append(element('div', 'cell-detail', `至 ${formatDate(account.cooldown_until)}`));
      const totals = element('td');
      totals.dataset.label = '请求 / 失败';
      const counts = element('div', 'request-numbers');
      counts.append(document.createTextNode(number.format(account.requests || 0)), element('span', 'request-divider', '/'), element('span', account.failures ? 'failure-number' : 'muted', number.format(account.failures || 0)));
      totals.append(counts);
      const recent = element('td');
      recent.dataset.label = '最近调用';
      recent.append(element('div', '', formatDate(account.last_used)));
      if (account.last_used) recent.append(element('div', 'cell-detail mono', account.last_status ? `HTTP ${account.last_status}` : '尚无响应状态'));
      const actionsCell = element('td', 'align-right');
      actionsCell.dataset.label = '管理账号';
      const actions = element('div', 'row-actions');
      const toggle = element('button', `toggle${account.enabled ? ' on' : ''}`);
      toggle.type = 'button';
      toggle.setAttribute('role', 'switch');
      toggle.setAttribute('aria-checked', String(account.enabled));
      toggle.setAttribute('aria-label', `${account.enabled ? '停用' : '启用'} ${account.name}`);
      toggle.title = `${account.enabled ? '停用' : '启用'}账号`;
      toggle.addEventListener('click', () => toggleAccount(account, toggle));
      const edit = element('button', 'text-button', '编辑');
      edit.setAttribute('aria-label', `编辑 ${account.name}`);
      edit.addEventListener('click', () => openAccount(account));
      const remove = element('button', 'text-button delete', '删除');
      remove.setAttribute('aria-label', `删除 ${account.name}`);
      remove.addEventListener('click', () => openDelete(account));
      actions.append(toggle, edit, remove);
      actionsCell.append(actions);
      row.append(identity, statusCell, totals, recent, actionsCell);
      rows.append(row);
    }
    $('account-rows').replaceChildren(rows);
  }

  function renderCatalogStatus() {
    const catalog = state?.catalog;
    if (!catalog) return;
    $('catalog-updated').textContent = catalog.last_success ? `最近同步 ${formatDate(catalog.last_success)}` : '尚未成功同步 · 使用内置目录';
    $('catalog-count').textContent = `${number.format(catalog.count || 0)} MODELS`;
    $('catalog-error').hidden = !catalog.last_error;
    $('catalog-error').textContent = catalog.last_error ? `最近一次同步未完成，继续使用已有目录。${catalog.last_error}` : '';
  }

  async function loadState({ quiet = false } = {}) {
    const generation = authGeneration;
    if (stateLoadingGeneration === generation) return;
    stateLoadingGeneration = generation;
    try {
      const result = await api('/state');
      if (generation !== authGeneration) return;
      state = result;
      csrfToken = result.csrf_token;
      showApp();
      renderState();
    } catch (error) {
      if (generation !== authGeneration) return;
      if (error.status !== 401) {
        $('service-dot').className = 'status-dot offline';
        $('service-label').textContent = '暂时无法连接';
        if (!quiet) notify(error.message, true);
        if (!authenticated) formError('login-error', '无法连接管理服务，请检查服务是否运行。');
      }
    } finally {
      if (stateLoadingGeneration === generation) stateLoadingGeneration = null;
    }
  }

  async function toggleAccount(account, button) {
    button.disabled = true;
    try {
      await api(`/accounts/${encodeURIComponent(account.id)}`, { method: 'PATCH', body: { enabled: !account.enabled } });
      notify(`${account.name} 已${account.enabled ? '停用' : '启用'}`);
      await loadState();
    } catch (error) { if (error.status !== 401) notify(error.message, true); }
    finally { button.disabled = false; }
  }

  function openAccount(account = null) {
    editingID = account?.id || null;
    $('account-form').reset();
    $('account-dialog-title').textContent = account ? '编辑账号' : '添加账号';
    $('account-name').value = account?.name || '';
    $('account-id').value = account?.account_id || '';
    $('account-enabled').checked = account ? account.enabled : true;
    $('account-token').required = !account;
    $('account-token').placeholder = account ? '留空保留原密钥' : '输入 Cloudflare API Token';
    $('token-optional').hidden = !account;
    formError('account-error');
    $('account-dialog').showModal();
    $('account-name').focus();
  }

  function openDelete(account) {
    deletingID = account.id;
    $('delete-name').textContent = account.name;
    formError('delete-error');
    $('delete-dialog').showModal();
    $('delete-dialog').querySelector('[data-close-dialog]').focus();
  }

  async function loadModels() {
    const generation = authGeneration;
    if (modelsLoadingGeneration === generation) return;
    modelsLoadingGeneration = generation;
    $('model-loading').hidden = models !== null;
    try {
      const result = await api('/models');
      if (generation !== authGeneration) return;
      models = result.data || [];
      const previousTask = $('model-task').value;
      const tasks = [...new Set(models.flatMap((model) => model.tasks || []))].sort();
      const options = document.createDocumentFragment();
      const all = element('option', '', '全部任务类型');
      all.value = '';
      options.append(all);
      for (const task of tasks) {
        const option = element('option', '', task);
        option.value = task;
        options.append(option);
      }
      $('model-task').replaceChildren(options);
      if (tasks.includes(previousTask)) $('model-task').value = previousTask;
      renderModels();
      updateObservationFilters();
    } catch (error) {
      if (generation !== authGeneration) return;
      if (error.status !== 401) {
        notify(error.message, true);
        if (models === null) {
          $('model-list').replaceChildren(element('p', 'notice warning', '模型目录加载失败，请点击“同步目录”重试。'));
          $('model-empty').hidden = true;
        }
      }
    } finally {
      if (modelsLoadingGeneration === generation) modelsLoadingGeneration = null;
      if (generation === authGeneration) $('model-loading').hidden = true;
    }
  }

  function renderModels() {
    if (models === null) return;
    const query = $('model-search').value.trim().toLowerCase();
    const task = $('model-task').value;
    const filtered = models.filter((model) => (!query || `${model.id} ${model.upstream_id || ''}`.toLowerCase().includes(query)) && (!task || (model.tasks || []).includes(task)));
    $('model-match-count').textContent = `${filtered.length} / ${models.length} 个模型`;
    $('model-empty').hidden = filtered.length !== 0;
    const list = document.createDocumentFragment();
    for (const model of filtered) {
      const row = element('article', 'model-row');
      const info = element('div');
      info.append(element('h2', 'model-name', model.id), element('p', 'model-upstream', model.upstream_id || model.id));
      const tags = element('div', 'task-tags');
      for (const taskName of model.tasks || []) tags.append(element('span', 'task-tag', taskName));
      info.append(tags);
      const endpoints = element('div', 'model-endpoints');
      if (model.supported_endpoints?.length) {
        for (const endpoint of model.supported_endpoints) endpoints.append(element('code', 'endpoint', endpoint));
      } else endpoints.append(element('span', 'unsupported', '尚无兼容入口'));
      row.append(info, endpoints);
      list.append(row);
    }
    $('model-list').replaceChildren(list);
  }

  function switchTab(tab) {
    currentTab = tab;
    for (const name of ['overview', 'logs', 'accounts', 'models']) $(`${name}-view`).hidden = tab !== name;
    $('breadcrumb').textContent = { overview: '统计概览', logs: '请求日志', accounts: '账号池', models: '模型目录' }[tab];
    for (const button of document.querySelectorAll('[data-tab]')) {
      const selected = button.dataset.tab === tab;
      button.classList.toggle('selected', selected);
      if (selected) button.setAttribute('aria-current', 'page');
      else button.removeAttribute('aria-current');
    }
    if (tab === 'models') loadModels();
    refreshActiveObservation();
  }

  $('login-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const generation = ++authGeneration;
    const button = $('login-submit');
    button.disabled = true;
    formError('login-error');
    const token = $('auth-token').value.trim();
    try {
      const result = await api('/session', { method: 'POST', loginToken: token });
      if (generation !== authGeneration) return;
      csrfToken = result.csrf_token;
      $('auth-token').value = '';
      await loadState();
      if (authenticated && currentTab === 'models') await loadModels();
    } catch (error) {
      if (generation === authGeneration && !error.stale) formError('login-error', error.status === 401 ? '管理密钥不正确，请检查 AUTH_TOKEN。' : error.message);
    }
    finally { button.disabled = false; }
  });

  $('logout').addEventListener('click', async () => {
    authGeneration++;
    $('logout').disabled = true;
    try {
      await api('/session', { method: 'DELETE' });
      showLogin();
      $('auth-token').focus();
    } catch (error) { if (error.status !== 401) notify(error.message, true); }
    finally { $('logout').disabled = false; }
  });

  $('add-account').addEventListener('click', () => openAccount());
  $('empty-add-account').addEventListener('click', () => openAccount());
  for (const button of document.querySelectorAll('[data-close-dialog]')) button.addEventListener('click', () => $(button.dataset.closeDialog).close());
  $('account-dialog').addEventListener('close', () => { $('account-token').value = ''; });

  $('account-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const button = $('save-account');
    button.disabled = true;
    button.textContent = '保存中…';
    formError('account-error');
    const body = {
      name: $('account-name').value.trim(),
      account_id: $('account-id').value.trim(),
      api_token: $('account-token').value.trim(),
      enabled: $('account-enabled').checked,
    };
    try {
      await api(editingID ? `/accounts/${encodeURIComponent(editingID)}` : '/accounts', { method: editingID ? 'PATCH' : 'POST', body });
      $('account-dialog').close();
      notify(editingID ? '账号修改已保存' : '账号已添加，配置立即生效');
      await loadState();
    } catch (error) { if (error.status !== 401) formError('account-error', error.message); }
    finally { button.disabled = false; button.textContent = '保存账号'; }
  });

  $('delete-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    if (!deletingID) return;
    $('delete-submit').disabled = true;
    formError('delete-error');
    try {
      await api(`/accounts/${encodeURIComponent(deletingID)}`, { method: 'DELETE' });
      $('delete-dialog').close();
      notify('账号已删除');
      deletingID = null;
      await loadState();
    } catch (error) { if (error.status !== 401) formError('delete-error', error.message); }
    finally { $('delete-submit').disabled = false; }
  });

  for (const button of document.querySelectorAll('[data-tab]')) button.addEventListener('click', () => switchTab(button.dataset.tab));
  $('model-search').addEventListener('input', renderModels);
  $('model-task').addEventListener('change', renderModels);
  $('refresh-models').addEventListener('click', async () => {
    const button = $('refresh-models');
    button.disabled = true;
    button.textContent = '同步中…';
    try {
      await api('/models/refresh', { method: 'POST' });
      await loadState();
      if (authenticated) await loadModels();
      if (authenticated) notify('模型目录已同步');
    } catch (error) { if (error.status !== 401) notify(error.message, true); }
    finally {
      button.disabled = false;
      button.replaceChildren(element('span', '', '↻'), document.createTextNode(' 同步目录'));
    }
  });

  $('copy-base').addEventListener('click', async () => {
    try {
      if (!navigator.clipboard) throw new Error('当前浏览器不支持自动复制，请选择地址手动复制。');
      await navigator.clipboard.writeText(`${location.origin}/v1`);
      $('copy-base').textContent = '已复制';
      notify('API 地址已复制');
      setTimeout(() => { $('copy-base').textContent = '复制'; }, 2000);
    } catch (error) { notify(error.message || '复制失败，请选择地址手动复制。', true); }
  });

  let logsPage = 1;
  const observationVersion = { stats: 0, logs: 0 };
  const observationLoaded = { stats: false, logs: false };
  const knownModels = new Map();
  const knownAccounts = new Map();
  let detailVersion = 0;
  let selectedLog = null;
  let searchTimer;
  let exportPending = false;
  const outcomeLabels = { success: '成功', error: '失败', canceled: '已取消', interrupted: '已中断' };

  function bytes(value) {
    const n = Number(value) || 0;
    if (n < 1024) return `${number.format(n)} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
    return `${(n / 1024 / 1024).toFixed(1)} MiB`;
  }

  function duration(value) {
    if (value === null || value === undefined) return '—';
    return value >= 1000 ? `${(value / 1000).toFixed(2)} s` : `${number.format(Math.round(value))} ms`;
  }

  function filters(prefix) {
    const query = new URLSearchParams();
    for (const name of ['range', 'model', 'account', 'status', 'q']) {
      const value = $(`${prefix}-${name}`)?.value.trim();
      if (value) query.set(name, value);
    }
    return query;
  }

  function populateFilter(select, items, caption) {
    const previous = select.value;
    const sorted = [...items].sort((a, b) => a[1].localeCompare(b[1]));
    const signature = JSON.stringify([sorted, previous && !items.has(previous) ? previous : '']);
    if (select.dataset.optionsSignature === signature) return;
    select.dataset.optionsSignature = signature;
    const options = [element('option', '', caption)];
    options[0].value = '';
    for (const [value, label] of sorted) {
      const option = element('option', '', label);
      option.value = value;
      options.push(option);
    }
    if (previous && !items.has(previous)) { const option = element('option', '', previous); option.value = previous; options.push(option); }
    select.replaceChildren(...options);
    select.value = previous;
  }

  function updateObservationFilters() {
    for (const model of models || []) knownModels.set(model.id, model.id);
    for (const account of state?.accounts || []) knownAccounts.set(account.id, account.name);
    for (const prefix of ['stats', 'logs']) {
      populateFilter($(`${prefix}-model`), knownModels, '全部模型');
      populateFilter($(`${prefix}-account`), knownAccounts, '全部账号');
    }
  }

  function retention(prefix, storage) {
    $(`${prefix}-storage`).textContent = storage ? `最多保留 ${storage.retention_days} 天 / ${number.format(storage.max_records)} 条 / ${bytes(storage.max_bytes)}，达到上限后自动清理最早记录。当前 ${number.format(storage.records)} 条 · ${bytes(storage.bytes)}。` : '';
    if (storage?.last_error) {
      $(`${prefix}-error`).hidden = false;
      $(`${prefix}-error`).textContent = `日志存储异常：${storage.last_error}`;
    }
  }

  async function loadObservation(prefix, { quiet = false } = {}) {
    const generation = authGeneration;
    const version = ++observationVersion[prefix];
    const query = filters(prefix);
    if (prefix === 'logs') { query.set('page', String(logsPage)); query.set('page_size', '25'); }
    if (!quiet) $(`${prefix}-loading`).hidden = false;
    try {
      const result = await api(`/${prefix}?${query}`);
      if (generation !== authGeneration || version !== observationVersion[prefix]) return;
      $(`${prefix}-error`).hidden = true;
      observationLoaded[prefix] = true;
      if (prefix === 'stats') renderStats(result); else renderLogs(result);
      retention(prefix, result.storage);
      updateObservationFilters();
    } catch (error) {
      if (generation !== authGeneration || version !== observationVersion[prefix] || error.status === 401) return;
      $(`${prefix}-error`).textContent = `加载失败：${error.message}${observationLoaded[prefix] ? '，目前显示上次成功获取的数据。' : '，请调整筛选或稍后重试。'}`;
      $(`${prefix}-error`).hidden = false;
    } finally {
      if (generation === authGeneration && version === observationVersion[prefix]) $(`${prefix}-loading`).hidden = true;
    }
  }

  function renderStats(data) {
    const s = data.summary || {};
    $('stats-content').hidden = false;
    $('stats-requests').textContent = number.format(s.requests || 0);
    $('stats-results').textContent = `${number.format(s.success || 0)} 成功 · ${number.format(s.errors || 0)} 失败 · ${number.format((s.canceled || 0) + (s.interrupted || 0))} 取消/中断`;
    $('stats-rate').replaceChildren(document.createTextNode(Number(s.success_rate || 0).toFixed(1)), element('span', '', '%'));
    $('stats-active').textContent = `${number.format(data.active_requests || 0)} 个请求正在处理中（全部）`;
    $('stats-duration').replaceChildren(document.createTextNode(s.avg_duration_ms >= 1000 ? (s.avg_duration_ms / 1000).toFixed(2) : String(Math.round(s.avg_duration_ms || 0))), element('span', '', s.avg_duration_ms >= 1000 ? 's' : 'ms'));
    $('stats-p95').textContent = `P95 ${duration(s.p95_duration_ms || 0)}`;
    $('stats-tokens').textContent = number.format(s.total_tokens || 0);
    $('stats-coverage').textContent = `${number.format(s.usage_reported_requests || 0)} / ${number.format(s.requests || 0)} 个请求报告用量`;
    $('stats-token-split').textContent = `${number.format(s.input_tokens || 0)} / ${number.format(s.output_tokens || 0)}`;
    $('stats-token-extra').textContent = `${number.format(s.cached_tokens || 0)} / ${number.format(s.reasoning_tokens || 0)}`;
    $('stats-ttfb').textContent = duration(s.avg_ttfb_ms);
    $('stats-bytes').textContent = `${bytes(s.request_bytes)} / ${bytes(s.response_bytes)}`;
    renderTraffic(data.timeline || []);
    renderBreakdown('stats-models', data.by_model || [], false);
    renderBreakdown('stats-accounts', data.by_account || [], true);
  }

  function svgNode(tag, attributes = {}) {
    const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
    for (const [name, value] of Object.entries(attributes)) node.setAttribute(name, String(value));
    return node;
  }

  function renderTraffic(timeline) {
    const chart = $('traffic-chart');
    chart.replaceChildren();
    if (!timeline.length || !timeline.some(item => item.requests)) {
      chart.append(element('p', 'chart-empty', '此时间范围内暂无请求；开始调用后即可查看趋势。'));
      $('traffic-start').textContent = ''; $('traffic-end').textContent = ''; $('traffic-caption').textContent = '';
      return;
    }
    const max = Math.max(1, ...timeline.map(item => item.requests || 0));
    const svg = svgNode('svg', { viewBox: '0 0 960 185', role: 'img', 'aria-label': `请求趋势，${timeline.length} 个时段，单个时段最多 ${max} 次请求`, preserveAspectRatio: 'none' });
    for (let i = 0; i < 4; i++) {
      const y = 10 + i * 50;
      svg.append(svgNode('line', { x1: 35, x2: 955, y1: y, y2: y, class: 'chart-gridline' }));
      const label = svgNode('text', { x: 0, y: y + 4, class: 'chart-tick' });
      label.textContent = String(Math.round(max * (1 - i / 3))); svg.append(label);
    }
    const width = 914 / timeline.length;
    timeline.forEach((bucket, index) => {
      const successHeight = (bucket.success || 0) / max * 150;
      const other = Math.max(0, (bucket.requests || 0) - (bucket.success || 0));
      const otherHeight = other / max * 150;
      const group = svgNode('g');
      const title = svgNode('title');
      title.textContent = `${formatDate(bucket.time)}：${bucket.requests || 0} 次请求，${bucket.success || 0} 成功，${other} 未完成/失败`;
      group.append(title);
      group.append(svgNode('rect', { x: 38 + index * width, y: 160 - successHeight, width: Math.max(1, width - 7), height: successHeight, class: 'chart-bar-success' }));
      group.append(svgNode('rect', { x: 38 + index * width, y: 160 - successHeight - otherHeight, width: Math.max(1, width - 7), height: otherHeight, class: 'chart-bar-error' }));
      svg.append(group);
    });
    chart.append(svg);
    $('traffic-start').textContent = formatDate(timeline[0].time);
    $('traffic-end').textContent = formatDate(timeline[timeline.length - 1].time);
    $('traffic-caption').textContent = $('stats-range').value === '24h' ? '每小时请求数' : '每天请求数';
  }

  function renderBreakdown(id, items, accounts) {
    const holder = $(id); holder.replaceChildren();
    if (!items.length) { holder.append(element('p', 'breakdown-empty', '暂无请求数据')); return; }
    const table = element('table', 'breakdown-table');
    const head = element('thead'); const hr = element('tr');
    for (const label of [accounts ? '账号' : '模型', '请求', '成功率', 'TOKEN']) hr.append(element('th', '', label));
    head.append(hr); table.append(head);
    const body = element('tbody');
    for (const item of items) {
      if (accounts && item.key) knownAccounts.set(item.key, item.name || item.key);
      if (!accounts && item.key) knownModels.set(item.key, item.key);
      const row = element('tr');
      const name = element('td', accounts ? '' : 'mono', accounts ? (item.name || '未分配账号') : (item.key || '未指定模型'));
      name.title = item.key || '';
      row.append(name, element('td', 'mono', number.format(item.requests || 0)), element('td', 'mono', `${Number(item.success_rate || 0).toFixed(1)}%`), element('td', 'mono', number.format(item.total_tokens || 0)));
      body.append(row);
    }
    table.append(body); const wrap = element('div', 'table-wrap'); wrap.append(table); holder.append(wrap);
  }

  function renderLogs(data) {
    const lastPage = Math.max(1, Math.ceil((data.total || 0) / (data.page_size || 25)));
    if ((data.page || logsPage) > lastPage) {
      logsPage = lastPage;
      loadObservation('logs');
      return;
    }
    const rows = document.createDocumentFragment();
    for (const log of data.data || []) {
      if (log.model) knownModels.set(log.model, log.model);
      if (log.account_id) knownAccounts.set(log.account_id, log.account_name || log.account_id);
      const row = element('tr');
      const when = element('td'); when.dataset.label = '时间 / ID';
      when.append(element('div', '', formatDate(log.started_at)), element('div', 'cell-detail mono log-short-id', log.id));
      when.lastChild.title = log.id;
      const model = element('td'); model.dataset.label = '模型 / 入口';
      model.append(element('div', 'log-model mono', log.model || '未指定模型'), element('div', 'cell-detail mono', `${log.method} ${log.path}`));
      const account = element('td', '', log.account_name || '未分配'); account.dataset.label = '账号';
      const status = element('td'); status.dataset.label = '结果';
      status.append(element('span', `badge outcome-${log.outcome}`, outcomeLabels[log.outcome] || log.outcome), element('div', 'cell-detail mono', `HTTP ${log.status_code}${log.stream ? ' · STREAM' : ''}`));
      const time = element('td'); time.dataset.label = '耗时 / 首字节';
      time.append(element('div', 'mono', duration(log.duration_ms)), element('div', 'cell-detail mono', duration(log.ttfb_ms)));
      const tokens = element('td', 'mono', log.usage?.reported ? number.format(log.usage.total_tokens || 0) : '未报告'); tokens.dataset.label = 'TOKEN';
      const action = element('td', 'align-right'); const button = element('button', 'text-button detail-button', '查看 →');
      button.setAttribute('aria-label', `查看请求 ${log.id}`); button.addEventListener('click', () => openLog(log.id)); action.append(button);
      row.append(when, model, account, status, time, tokens, action); rows.append(row);
    }
    $('logs-rows').replaceChildren(rows);
    $('logs-empty').hidden = Boolean(data.data?.length);
    $('logs-rows').closest('.table-wrap').hidden = !data.data?.length;
    $('logs-count').textContent = `${number.format(data.total || 0)} 条匹配 · ${number.format(data.active_requests || 0)} 个处理中`;
    const pages = Math.max(1, Math.ceil((data.total || 0) / (data.page_size || 25)));
    logsPage = data.page || logsPage;
    $('logs-page-label').textContent = `第 ${logsPage} / ${pages} 页 · 每页 ${data.page_size || 25} 条`;
    $('logs-prev').disabled = logsPage <= 1;
    $('logs-next').disabled = logsPage >= pages;
  }

  function prettyText(snapshot) {
    if (!snapshot?.text) return snapshot?.kind === 'empty' ? '（空）' : '（无可显示文本；媒体内容见摘要）';
    if (snapshot.kind === 'json') { try { return JSON.stringify(JSON.parse(snapshot.text), null, 2); } catch (_) { /* A truncated JSON payload remains plain text. */ } }
    return snapshot.text;
  }

  function snapshotView(title, snapshot = {}) {
    const section = element('section', 'snapshot-section');
    const heading = element('div', 'section-heading');
    heading.append(element('h3', '', title), element('span', 'section-note mono', `${snapshot.content_type || snapshot.kind || 'empty'} · ${bytes(snapshot.size)}`));
    section.append(heading);
    if (snapshot.url) section.append(element('p', 'snapshot-url mono', snapshot.url));
    if (snapshot.truncated) section.append(element('p', 'notice warning', '此内容超过记录上限，已截断。以下为保留部分。'));
    const headers = element('details', 'snapshot-headers');
    headers.append(element('summary', '', '查看请求头 / 响应头'), element('pre', 'payload', JSON.stringify(snapshot.headers || {}, null, 2)));
    section.append(headers);
    const pre = element('pre', 'payload', prettyText(snapshot)); pre.tabIndex = 0; section.append(pre);
    return section;
  }

  function metadataItem(label, value) { const node = element('div'); node.append(element('dt', '', label), element('dd', '', value)); return node; }

  function renderLogDetail(log) {
    const content = $('detail-content'); content.replaceChildren();
    const meta = element('dl', 'detail-metadata');
    for (const [label, value] of [
      ['请求入口', `${log.method} ${log.path}`], ['模型', log.model || '未指定'], ['上游模型', log.upstream_model || '—'],
      ['账号', log.account_name || '未分配'], ['账号记录 ID', log.account_id || '—'], ['开始时间', timestamp(log.started_at)?.toLocaleString('zh-CN', { hour12: false }) || '—'],
      ['结束时间', timestamp(log.finished_at)?.toLocaleString('zh-CN', { hour12: false }) || '—'],
      ['结果', `${outcomeLabels[log.outcome] || log.outcome} · HTTP ${log.status_code}${log.stream ? ' · 流式' : ''}`],
      ['总耗时 / 首字节', `${duration(log.duration_ms)} / ${duration(log.ttfb_ms)}`], ['上游调用', String(log.upstream_calls || 0)],
      ['输入 / 输出 Token', log.usage?.reported ? `${number.format(log.usage.input_tokens || 0)} / ${number.format(log.usage.output_tokens || 0)}` : '上游未报告用量'],
      ['缓存 / 推理 Token', log.usage?.reported ? `${number.format(log.usage.cached_tokens || 0)} / ${number.format(log.usage.reasoning_tokens || 0)}` : '—'],
      ['总 Token', log.usage?.reported ? number.format(log.usage.total_tokens || 0) : '—'],
    ]) meta.append(metadataItem(label, value));
    content.append(meta);
    if (log.error) content.append(element('p', 'notice warning', log.error));
    content.append(element('p', 'detail-note', '凭据已脱敏；二进制和 Base64 媒体保留摘要。文本与 JSON 可直接选中复制。'));
    content.append(snapshotView('客户端请求', log.request), snapshotView('客户端响应', log.response));
    const upstream = element('section', 'upstream-section'); upstream.append(element('h3', '', `上游调用 · ${log.upstream?.length || 0}`));
    for (const [index, hop] of (log.upstream || []).entries()) {
      const details = element('details', 'upstream-hop');
      details.append(element('summary', 'mono', `${index + 1}. ${hop.path} · HTTP ${hop.status_code} · ${duration(hop.duration_ms)}`));
      if (hop.error) details.append(element('p', 'notice warning', hop.error));
      details.append(snapshotView('上游请求', hop.request), snapshotView('上游响应', hop.response)); upstream.append(details);
    }
    if (!log.upstream?.length) upstream.append(element('p', 'table-footnote', '此请求未发起上游调用。'));
    content.append(upstream);
  }

  async function openLog(id) {
    const generation = authGeneration;
    const version = ++detailVersion;
    selectedLog = null;
    $('detail-id').textContent = id;
    $('detail-content').replaceChildren(); $('detail-error').hidden = true;
    $('detail-loading').hidden = false; $('download-log').disabled = true;
    if (!$('log-dialog').open) $('log-dialog').showModal();
    try {
      const log = await api(`/logs/${encodeURIComponent(id)}`);
      if (generation !== authGeneration || version !== detailVersion || !$('log-dialog').open) return;
      selectedLog = log; renderLogDetail(log); $('download-log').disabled = false;
    } catch (error) {
      if (generation !== authGeneration || version !== detailVersion || error.status === 401) return;
      $('detail-error').textContent = error.status === 404 ? '该日志已过期或被存储上限自动清理。' : error.message;
      $('detail-error').hidden = false;
    } finally { if (generation === authGeneration && version === detailVersion) $('detail-loading').hidden = true; }
  }

  function downloadBlob(blob, filename) {
    const url = URL.createObjectURL(blob); const a = element('a');
    a.href = url; a.download = filename; document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
  }

  async function exportLogs() {
    if (exportPending) return;
    const generation = authGeneration;
    exportPending = true; $('export-logs').disabled = true; $('export-logs').textContent = '正在导出…';
    try {
      const response = await fetch(`/admin/api/logs/export?${filters('logs')}`, { credentials: 'same-origin', cache: 'no-store' });
      if (generation !== authGeneration) return;
      if (response.status === 401) { showLogin('登录已失效，请重新输入管理密钥。'); return; }
      if (!response.ok) { let message = `导出失败（HTTP ${response.status}）`; try { message = (await response.json()).error?.message || message; } catch (_) {} throw new Error(message); }
      const blob = await response.blob();
      if (generation !== authGeneration) return;
      downloadBlob(blob, `workersai-logs-${new Date().toISOString().slice(0, 10)}.ndjson`);
      notify('筛选日志已导出');
    } catch (error) { if (generation === authGeneration) notify(error.message, true); }
    finally { exportPending = false; $('export-logs').disabled = false; $('export-logs').textContent = '↓ 导出筛选日志'; }
  }

  function refreshActiveObservation(options = {}) {
    if (currentTab === 'overview') loadObservation('stats', options);
    if (currentTab === 'logs') loadObservation('logs', options);
  }

  for (const prefix of ['stats', 'logs']) {
    for (const control of document.querySelectorAll(`[data-filter-set="${prefix}"] select`)) control.addEventListener('change', () => {
      if (prefix === 'logs') logsPage = 1;
      loadObservation(prefix);
    });
  }
  $('logs-q').addEventListener('input', () => {
    clearTimeout(searchTimer); logsPage = 1; observationVersion.logs++;
    searchTimer = setTimeout(() => { if (authenticated) loadObservation('logs'); }, 300);
  });
  $('logs-prev').addEventListener('click', () => { if (logsPage > 1) { logsPage--; loadObservation('logs'); } });
  $('logs-next').addEventListener('click', () => { logsPage++; loadObservation('logs'); });
  $('export-logs').addEventListener('click', exportLogs);
  $('log-dialog').addEventListener('close', () => { detailVersion++; selectedLog = null; $('detail-content').replaceChildren(); });
  $('download-log').addEventListener('click', () => { if (selectedLog) downloadBlob(new Blob([JSON.stringify(selectedLog, null, 2)], { type: 'application/json' }), `request-${selectedLog.id}.json`); });
  $('copy-log-id').addEventListener('click', async () => {
    try { if (!navigator.clipboard) throw new Error('当前浏览器无法自动复制，请选中请求 ID 手动复制。'); await navigator.clipboard.writeText($('detail-id').textContent); notify('请求 ID 已复制'); }
    catch (error) { notify(error.message, true); }
  });


  setInterval(async () => {
    if (!authenticated || document.visibilityState === 'hidden') return;
    const previousSuccess = state?.catalog?.last_success;
    await loadState({ quiet: true });
    if (authenticated) refreshActiveObservation({ quiet: true });
    if (authenticated && currentTab === 'models' && previousSuccess !== state?.catalog?.last_success) await loadModels();
  }, 15000);
  document.addEventListener('visibilitychange', () => {
    if (authenticated && document.visibilityState === 'visible') { loadState({ quiet: true }); refreshActiveObservation({ quiet: true }); }
  });
  loadState({ quiet: true });
})();
