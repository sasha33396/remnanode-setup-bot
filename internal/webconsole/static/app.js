(() => {
  'use strict';
  const state = { csrf: '', nodes: [], filter: 'all', query: '', selected: null, passwordAction: 'ssh', socket: null, terminal: null };
  const $ = (selector) => document.querySelector(selector);
  const nodesEl = $('#nodes');
  const notice = $('#notice');
  const passwordDialog = $('#password-dialog');
  const terminalDialog = $('#terminal-dialog');

  async function request(path, options = {}) {
    options.headers = { ...(options.headers || {}) };
    if (state.csrf && options.method && options.method !== 'GET') options.headers['X-CSRF-Token'] = state.csrf;
    const response = await fetch(path, options);
    if (response.status === 401) { location.href = '/login'; throw new Error('Сессия завершена'); }
    if (!response.ok) {
      let message = 'Ошибка запроса';
      try { message = (await response.json()).error || message; } catch (_) {}
      throw new Error(message);
    }
    return response.status === 204 ? null : response.json();
  }

  function escapeHTML(value) {
    return (value == null ? '' : String(value)).replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[char]));
  }

  function showNotice(message) { notice.textContent = message; notice.classList.remove('hidden'); }
  function clearNotice() { notice.classList.add('hidden'); notice.textContent = ''; }

  function render() {
    const query = state.query.toLowerCase();
    const visible = state.nodes.filter((node) => {
      if (state.filter === 'managed' && !node.managed) return false;
      if (state.filter === 'legacy' && node.managed) return false;
      return !query || `${node.name} ${node.address} ${(node.networkAddresses || []).join(' ')} ${node.panelName}`.toLowerCase().includes(query);
    });
    $('#total').textContent = state.nodes.length;
    $('#managed').textContent = state.nodes.filter((node) => node.managed).length;
    $('#legacy').textContent = state.nodes.filter((node) => !node.managed).length;
    if (!visible.length) { nodesEl.innerHTML = '<div class="empty">Подходящих нод нет</div>'; return; }
    nodesEl.innerHTML = visible.map((node) => `
      <article class="node-card">
        <div class="node-top"><div class="node-name" title="${escapeHTML(node.name)}">${escapeHTML(node.name)}</div><span class="status-dot ${escapeHTML(node.status)}" title="${escapeHTML(node.status)}"></span></div>
        <div class="node-address">${escapeHTML(node.address)}</div>
        <div class="node-address">Netplan: ${escapeHTML((node.networkAddresses || []).join(', ') || 'ещё не опрошен')}</div>
        <div class="node-meta"><span class="badge ${node.managed ? 'managed' : 'legacy'}">${node.managed ? 'Актуальная' : 'Legacy'}</span><span class="badge">${node.onlineKnown ? `${node.online} онлайн` : 'онлайн неизвестен'}</span>${node.disabled ? '<span class="badge">выключена</span>' : ''}</div>
        <div class="node-footer"><span class="panel-name">${escapeHTML(node.panelName)}</span><button class="button secondary actualize" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">Актуализировать</button><button class="button secondary connect" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">SSH</button></div>
      </article>`).join('');
  }

  async function loadNodes() {
    clearNotice(); $('#refresh').disabled = true;
    try { state.nodes = (await request('/api/nodes')).nodes || []; render(); }
    catch (error) { showNotice(error.message); }
    finally { $('#refresh').disabled = false; }
  }

  function selectNode(panel, uuid) { return state.nodes.find((node) => node.panelId === panel && node.uuid === uuid); }

  async function createTicket(node, password = '') {
    clearNotice();
    if (typeof Terminal !== 'function') throw new Error('Компонент терминала не загрузился. Обновите страницу.');
    const path = `/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/ticket`;
    const result = await request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) });
    openTerminal(node, result.ticket);
  }

  async function actualizeNode(node, password = '') {
    clearNotice();
    const path = `/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/actualize`;
    const result = await request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) });
    await loadNodes();
    showNotice(`${result.nodeName}: настройки актуализированы, IP: ${(result.addresses || []).join(', ') || 'не найдены'}, firewall: ${result.firewallChanged ? 'обновлён' : 'уже актуален'}`);
  }

  async function actualizeManagedNodes() {
    const button = $('#actualize-managed');
    if (!confirm('Актуализировать netplan и firewall на всех актуальных нодах?')) return;
    clearNotice(); button.disabled = true; button.textContent = 'Актуализация…';
    try {
      const result = await request('/api/nodes/actualize-managed', { method: 'POST' });
      await loadNodes();
      const failures = (result.failures || []).slice(0, 5).map((item) => `${item.PanelName}: ${item.NodeName} (${item.Reason})`).join('; ');
      showNotice(`Массовая актуализация завершена: ${result.updated}/${result.total}, ошибок: ${result.failed}, firewall изменён: ${result.firewallChanged}.${failures ? ` Ошибки: ${failures}` : ''}`);
    } catch (error) { showNotice(error.message); }
    finally { button.disabled = false; button.textContent = 'Актуализировать все'; }
  }

  function openTerminal(node, ticket) {
    terminalDialog.showModal();
    $('#terminal-title').textContent = node.name;
    $('#terminal-subtitle').textContent = `${node.address} · ${node.managed ? 'deploy key' : 'legacy password'}`;
    const terminal = new Terminal({ cursorBlink: true, convertEol: true, fontSize: 14, fontFamily: '"SFMono-Regular",Consolas,"Liberation Mono",monospace', theme: { background: '#080b10', foreground: '#d9e2ef', cursor: '#66e3b4', selectionBackground: '#314255' }, scrollback: 5000 });
    state.terminal = terminal; terminal.open($('#terminal'));
    const fit = () => { const cols = Math.max(40, Math.floor($('#terminal').clientWidth / 8.4)); const rows = Math.max(12, Math.floor($('#terminal').clientHeight / 17)); terminal.resize(cols, rows); return { cols, rows }; };
    const size = fit();
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${scheme}//${location.host}/ws/terminal?ticket=${encodeURIComponent(ticket)}&cols=${size.cols}&rows=${size.rows}`);
    state.socket = ws; ws.binaryType = 'arraybuffer';
    const decoder = new TextDecoder();
    ws.onopen = () => { terminal.focus(); terminal.onData((data) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: 'input', data }))); };
    ws.onmessage = (event) => { if (event.data instanceof ArrayBuffer) terminal.write(decoder.decode(event.data, { stream: true })); else { try { const value = JSON.parse(event.data); terminal.writeln(`\r\n${value.message || 'SSH-сессия завершена'}`); } catch (_) {} } };
    ws.onerror = () => terminal.writeln('\r\n\x1b[31mНе удалось установить WebSocket-соединение.\x1b[0m');
    ws.onclose = () => terminal.writeln('\r\n\x1b[90mSSH-сессия закрыта.\x1b[0m');
    let resizeTimer;
    const observer = new ResizeObserver(() => { clearTimeout(resizeTimer); resizeTimer = setTimeout(() => { if (!state.terminal) return; const next = fit(); if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols: next.cols, rows: next.rows })); }, 80); });
    observer.observe($('#terminal')); state.resizeObserver = observer;
  }

  function closeTerminal() {
    if (state.resizeObserver) state.resizeObserver.disconnect();
    if (state.socket) state.socket.close();
    if (state.terminal) state.terminal.dispose();
    state.socket = state.terminal = state.resizeObserver = null;
    terminalDialog.close(); $('#terminal').textContent = '';
  }

  document.addEventListener('click', async (event) => {
    const actualize = event.target.closest('.actualize');
    if (actualize) {
      const node = selectNode(actualize.dataset.panel, actualize.dataset.uuid); if (!node) return;
      state.selected = node; state.passwordAction = 'actualize';
      if (node.managed) { actualize.disabled = true; try { await actualizeNode(node); } catch (error) { showNotice(error.message); } finally { actualize.disabled = false; } }
      else { $('#password-title').textContent = `Актуализация: ${node.name}`; $('#password').value = ''; passwordDialog.showModal(); setTimeout(() => $('#password').focus(), 50); }
      return;
    }
    const connect = event.target.closest('.connect');
    if (connect) {
      const node = selectNode(connect.dataset.panel, connect.dataset.uuid); if (!node) return;
      state.selected = node; state.passwordAction = 'ssh';
      if (node.managed) { connect.disabled = true; try { await createTicket(node); } catch (error) { showNotice(error.message); } finally { connect.disabled = false; } }
      else { $('#password-title').textContent = node.name; $('#password').value = ''; passwordDialog.showModal(); setTimeout(() => $('#password').focus(), 50); }
    }
    if (event.target.matches('[data-close]')) { $('#password').value = ''; passwordDialog.close(); }
  });
  $('#password-form').addEventListener('submit', async (event) => { event.preventDefault(); const password = $('#password').value; $('#password').value = ''; passwordDialog.close(); try { if (state.passwordAction === 'actualize') await actualizeNode(state.selected, password); else await createTicket(state.selected, password); } catch (error) { showNotice(error.message); } });
  $('#terminal-close').addEventListener('click', closeTerminal);
  terminalDialog.addEventListener('cancel', (event) => { event.preventDefault(); closeTerminal(); });
  $('#refresh').addEventListener('click', loadNodes);
  $('#actualize-managed').addEventListener('click', actualizeManagedNodes);
  $('#search').addEventListener('input', (event) => { state.query = event.target.value; render(); });
  document.querySelectorAll('.filter').forEach((button) => button.addEventListener('click', () => { document.querySelectorAll('.filter').forEach((item) => item.classList.remove('active')); button.classList.add('active'); state.filter = button.dataset.filter; render(); }));
  $('#logout').addEventListener('click', async () => { try { await request('/logout', { method: 'POST' }); location.href = '/login'; } catch (error) { showNotice(error.message); } });

  (async () => { try { const session = await request('/api/session'); state.csrf = session.csrfToken; $('#operator').textContent = session.user.name || `Telegram ${session.user.id}`; await loadNodes(); } catch (error) { showNotice(error.message); } })();
})();
