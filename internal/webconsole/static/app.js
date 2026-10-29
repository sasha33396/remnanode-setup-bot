(() => {
  'use strict';
  const state = { csrf: '', nodes: [], panels: [], capabilities: {}, filter: 'all', query: '', selected: null, passwordAction: 'ssh', nodeOperation: '', bulkIPPayload: null, socket: null, terminal: null };
  const $ = (selector) => document.querySelector(selector);
  const nodesEl = $('#nodes');
  const notice = $('#notice');
  const jobProgress = $('#job-progress');
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

  function showJobProgress(title, progress = {}) {
    const percent = Math.max(0, Math.min(100, Number(progress.percent) || 0));
    $('#job-progress-title').textContent = title || 'Настройка ноды';
    $('#job-progress-percent').textContent = `${percent}%`;
    $('#job-progress-step').textContent = progress.message || 'Операция поставлена в очередь';
    $('#job-progress-bar').style.width = `${percent}%`;
    const track = jobProgress.querySelector('.job-progress-track');
    track.setAttribute('aria-valuenow', String(percent));
    jobProgress.classList.toggle('failed', progress.failed === true);
    jobProgress.classList.remove('hidden');
  }

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
        <div class="node-footer"><span class="panel-name">${escapeHTML(node.panelName)}</span>${node.managed ? '' : `<button class="button primary promote" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">Сделать актуальной</button>`}<div class="node-actions"><button class="button secondary actualize" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">Синхронизировать</button><button class="button secondary node-ip" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">Сменить IP</button><button class="button secondary node-move" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">Перенести</button><button class="button secondary node-dns" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">DNS</button><button class="button secondary connect" data-panel="${escapeHTML(node.panelId)}" data-uuid="${escapeHTML(node.uuid)}">SSH</button></div></div>
      </article>`).join('');
  }

  async function loadNodes() {
    clearNotice(); $('#refresh').disabled = true;
    try { state.nodes = (await request('/api/nodes')).nodes || []; render(); }
    catch (error) { showNotice(error.message); }
    finally { $('#refresh').disabled = false; }
  }

  function selectNode(panel, uuid) { return state.nodes.find((node) => node.panelId === panel && node.uuid === uuid); }

  const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

  async function waitForJob(jobId, message) {
    clearNotice();
    showJobProgress(message, { percent: 0, message: 'Получение состояния операции…' });
    for (;;) {
      const job = await request(`/api/jobs/${encodeURIComponent(jobId)}`);
      showJobProgress(message, job.progress || {});
      if (job.status === 'completed') return job.result || {};
      if (job.status === 'failed') {
        const error = job.error || 'Операция завершилась с ошибкой';
        const stage = job.progress && job.progress.message ? job.progress.message : '';
        showJobProgress(message, { ...(job.progress || {}), failed: true, message: stage ? `${error}. Последний этап: ${stage}` : error });
        throw new Error(error);
      }
      await delay(2000);
    }
  }

  async function createTicket(node, password = '') {
    clearNotice();
    if (typeof Terminal !== 'function') throw new Error('Компонент терминала не загрузился. Обновите страницу.');
    if (!node.managed) showNotice(`${node.name}: сначала перевожу legacy-ноду под управление, затем открою SSH по deploy-key…`);
    const path = `/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/ticket`;
    const result = await request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) });
    if (result.pending) {
      await waitForJob(result.jobId, `${node.name}: полный перевод выполняется в фоне. Не закрывайте страницу — SSH откроется автоматически…`);
      await loadNodes();
      const managed = selectNode(node.panelId, node.uuid);
      if (!managed || !managed.managed) throw new Error('Перевод завершён, но managed deployment не найден');
      return createTicket(managed);
    }
    await loadNodes();
    const updated = selectNode(node.panelId, node.uuid) || { ...node, managed: result.managed === true };
    openTerminal(updated, result.ticket);
  }

  async function actualizeNode(node, password = '') {
    clearNotice();
    const path = `/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/actualize`;
    let result = await request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) });
    if (result.pending) result = await waitForJob(result.jobId, `${node.name}: операция выполняется в фоне, повторно нажимать кнопку не нужно…`);
    await loadNodes();
    const action = result.adopted ? 'легаси-нода полностью переведена под управление и актуализирована' : 'настройки актуализированы';
    showNotice(`${result.nodeName}: ${action}, IP: ${(result.addresses || []).join(', ') || 'не найдены'}, firewall: ${result.firewallChanged ? 'обновлён' : 'проверен и актуален'}`);
  }

  async function actualizeManagedNodes() {
    const button = $('#actualize-managed');
    if (!confirm('Синхронизировать netplan и firewall на всех актуальных нодах?')) return;
    clearNotice(); button.disabled = true; button.textContent = 'Синхронизация…';
    try {
      const result = await request('/api/nodes/actualize-managed', { method: 'POST' });
      await loadNodes();
      const failures = (result.failures || []).slice(0, 5).map((item) => `${item.PanelName}: ${item.NodeName} (${item.Reason})`).join('; ');
      showNotice(`Массовая синхронизация завершена: ${result.updated}/${result.total}, ошибок: ${result.failed}, firewall изменён: ${result.firewallChanged}.${failures ? ` Ошибки: ${failures}` : ''}`);
    } catch (error) { showNotice(error.message); }
    finally { button.disabled = false; button.textContent = 'Синхронизировать все'; }
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

  function option(value, label) { return `<option value="${escapeHTML(value)}">${escapeHTML(label)}</option>`; }

  async function loadPanels() {
    const result = await request('/api/options/panels');
    state.panels = result.panels || [];
    const html = state.panels.map((panel) => option(panel.ID, panel.Name)).join('');
    $('#node-panel').innerHTML = html;
    $('#balancer-panel').innerHTML = html;
    $('#bulk-ip-panel').innerHTML = html;
    return state.panels;
  }

  async function loadCapabilities() { state.capabilities = await request('/api/options/capabilities'); }

  async function loadHosts(panelId) {
    const result = await request(`/api/options/hosts?panel=${encodeURIComponent(panelId)}`);
    $('#node-host').innerHTML = (result.hosts || []).map((host) => option(host.ID, `${host.Remark} — ${host.Address}`)).join('');
  }

  async function loadBalancerOptions(panelId) {
    const result = await request(`/api/options/balancer?panel=${encodeURIComponent(panelId)}`);
    $('#balancer-squad').innerHTML = (result.Squads || []).map((squad) => option(squad.ID, squad.Name)).join('');
    $('#balancer-tag').innerHTML = (result.Tags || []).map((tag) => option(tag, tag)).join('');
  }

  async function openAddNode() {
    clearNotice();
    try {
      if (!state.panels.length) await loadPanels();
      if (!state.panels.length) throw new Error('Нет доступных панелей');
      await loadHosts($('#node-panel').value);
      $('#add-node-dialog').showModal();
    } catch (error) { showNotice(error.message); }
  }

  async function openAddBalancer() {
    clearNotice();
    try {
      if (!state.panels.length) await loadPanels();
      if (!state.panels.length) throw new Error('Нет доступных панелей');
      await loadBalancerOptions($('#balancer-panel').value);
      $('#add-balancer-dialog').showModal();
    } catch (error) { showNotice(error.message); }
  }

  async function openBulkIP() {
    clearNotice();
    try {
      if (!state.panels.length) await loadPanels();
      if (!state.panels.length) throw new Error('Нет доступных панелей');
      state.bulkIPPayload = null;
      $('#bulk-ip-preview').classList.add('hidden');
      $('#bulk-ip-form button[type="submit"]').textContent = 'Проверить план';
      $('#bulk-ip-dialog').showModal();
    } catch (error) { showNotice(error.message); }
  }

  function bulkIPPayload() {
    const mappings = $('#bulk-ip-mappings').value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean).map((line) => {
      const parts = line.split(/[\s,;=]+/).filter(Boolean);
      if (parts.length !== 2) throw new Error(`Некорректная строка: ${line}`);
      return { oldIp: parts[0], newIp: parts[1] };
    });
    if (!mappings.length || mappings.length > 50) throw new Error('Нужно указать от 1 до 50 пар IP');
    return { panelId: $('#bulk-ip-panel').value, enableDisabled: $('#bulk-ip-enable').checked, mappings };
  }

  async function runOperation(path, body, title) {
    const response = await request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
    const result = response.pending ? await waitForJob(response.jobId, title) : response;
    await loadNodes();
    showNotice(result.warning || result.message || `${title}: операция успешно завершена`);
    return result;
  }

  async function openNodeOperation(kind, node) {
    state.selected = node; state.nodeOperation = kind; clearNotice();
    const title = $('#node-operation-title');
    const description = $('#node-operation-description');
    const fields = $('#node-operation-fields');
    if (kind === 'ip') {
      title.textContent = `Сменить IP: ${node.name}`;
      description.textContent = `Текущий адрес ${node.address}. Обычный режим обновляет Remnawave/DNS. Режим провайдера сначала настраивает IP на самом сервере, затем обновляет ноду.`;
      const modes = [option('panel', 'Remnawave + DNS')];
      if (state.capabilities.cherryIp) modes.push(option('cherry', 'Cherry Servers — floating IP'));
      if (state.capabilities.royalIp) modes.push(option('royal', 'Royal Hosting — основной IP'));
      fields.innerHTML = `<label>Способ<select id="operation-ip-mode">${modes.join('')}</select></label><label>Новый публичный IPv4<input id="operation-new-ip" required autocomplete="off"></label><label id="operation-ip-password-label" class="full hidden">Root-пароль сервера<input id="operation-ip-password" type="password" maxlength="1024" autocomplete="new-password"></label>`;
      $('#operation-ip-mode').addEventListener('change', (event) => { const needsPassword = event.target.value !== 'panel'; $('#operation-ip-password-label').classList.toggle('hidden', !needsPassword); $('#operation-ip-password').required = needsPassword; });
      $('#node-operation-submit').textContent = 'Сменить IP';
    } else if (kind === 'move') {
      title.textContent = `Перенести: ${node.name}`;
      description.textContent = 'Загружаю доступные Host…'; fields.innerHTML = '';
      const preview = await request(`/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/move`);
      const current = preview.target.currentHostKnown ? `${preview.target.currentHostRemark} (${preview.target.currentHostAddress})` : 'не определён';
      description.textContent = `Текущий Host: ${current}. На сервере будут переключены SNI и сертификат, затем профиль и inbound в Remnawave. IP ноды не изменится.`;
      fields.innerHTML = `<label>Новый Host<select id="operation-host" required>${(preview.hosts || []).map((host) => option(host.ID, `${host.Remark} — ${host.Address}`)).join('')}</select></label><label>Root-пароль ноды<input id="operation-password" type="password" required maxlength="1024" autocomplete="new-password"></label>`;
      $('#node-operation-submit').textContent = 'Перенести';
    } else {
      title.textContent = `Синхронизировать DNS: ${node.name}`;
      description.textContent = 'Проверяю DNS-зону…'; fields.innerHTML = '';
      const preview = await request(`/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/dns`);
      description.textContent = `${preview.note || 'DNS будет приведён к адресу из Remnawave.'} Зона: ${preview.dnsZone || 'не определена'}, адрес: ${preview.address}.`;
      if (!preview.canSync) throw new Error('DNS нельзя синхронизировать безопасно: целевая запись не определена');
      $('#node-operation-submit').textContent = 'Синхронизировать DNS';
    }
    $('#node-operation-dialog').showModal();
  }

  async function loadDeployments() {
    const result = await request('/api/deployments');
    const items = result.deployments || [];
    $('#deployment-details').classList.add('hidden');
    $('#deployments-list').innerHTML = items.length ? items.map((item) => `<button type="button" class="deployment-item" data-deployment="${escapeHTML(item.ID)}"><strong>${escapeHTML(item.NodeName || item.ID)}</strong><span>${escapeHTML(item.PanelName)} · ${escapeHTML(item.CurrentStep || item.Kind)}</span><span class="${item.Status === 'COMPLETED' ? 'success-text' : item.Status === 'FAILED' ? 'danger-text' : ''}">${escapeHTML(item.Status)}</span></button>`).join('') : '<div class="empty">Развёртываний пока нет</div>';
  }

  async function openDeployments() {
    clearNotice();
    try { await loadDeployments(); $('#deployments-dialog').showModal(); }
    catch (error) { showNotice(error.message); }
  }

  async function showDeployment(id) {
    const item = await request(`/api/deployments/${encodeURIComponent(id)}`);
    const actions = [];
    if (item.CanRepairCert) actions.push(['repair-certificate', 'Исправить сертификат и продолжить']);
    if (item.CanRetryDNS) actions.push(['dns', 'Повторить DNS']);
    if (item.CanRetryStep && !item.CanRepairCert) actions.push(['retry', 'Повторить шаг']);
    if (item.CanRecheck) actions.push(['recheck', 'Перепроверить ноду']);
    if (item.CanCancel) actions.push(['cancel', 'Отменить']);
    const details = $('#deployment-details');
    details.innerHTML = `<h3>${escapeHTML(item.NodeName || item.ID)}</h3><p class="muted">${escapeHTML(item.PanelName)} · ${escapeHTML(item.Kind)} · ${escapeHTML(item.Status)}</p><p>Текущий шаг: <strong>${escapeHTML(item.CurrentStep || '—')}</strong></p>${item.SafeError ? `<p class="danger-text">${escapeHTML(item.SafeError)}</p>` : ''}<div class="deployment-actions"><button type="button" class="button secondary deployment-logs" data-id="${escapeHTML(id)}">Журнал</button>${actions.map(([action,label]) => `<button type="button" class="button ${action === 'cancel' ? 'ghost' : 'secondary'} deployment-action" data-id="${escapeHTML(id)}" data-action="${action}">${escapeHTML(label)}</button>`).join('')}</div><div id="deployment-logs"></div>`;
    details.classList.remove('hidden');
  }

  async function showDeploymentLogs(id) {
    const result = await request(`/api/deployments/${encodeURIComponent(id)}/logs`);
    $('#deployment-logs').innerHTML = (result.logs || []).map((entry) => `<div class="deployment-log"><strong>${escapeHTML(entry.Step)}</strong> · ${escapeHTML(entry.Status)}<br><span class="muted">${escapeHTML(entry.Summary || entry.Code)}</span></div>`).join('') || '<p class="muted">Журнал пуст</p>';
  }

  document.addEventListener('click', async (event) => {
    const nodeIP = event.target.closest('.node-ip');
    const nodeMove = event.target.closest('.node-move');
    const nodeDNS = event.target.closest('.node-dns');
    if (nodeIP || nodeMove || nodeDNS) {
      const button = nodeIP || nodeMove || nodeDNS;
      const node = selectNode(button.dataset.panel, button.dataset.uuid); if (!node) return;
      button.disabled = true;
      try { await openNodeOperation(nodeIP ? 'ip' : nodeMove ? 'move' : 'dns', node); }
      catch (error) { showNotice(error.message); }
      finally { button.disabled = false; }
      return;
    }
    const deployment = event.target.closest('[data-deployment]');
    if (deployment) { try { await showDeployment(deployment.dataset.deployment); } catch (error) { showNotice(error.message); } return; }
    const deploymentLogs = event.target.closest('.deployment-logs');
    if (deploymentLogs) { try { await showDeploymentLogs(deploymentLogs.dataset.id); } catch (error) { showNotice(error.message); } return; }
    const deploymentAction = event.target.closest('.deployment-action');
    if (deploymentAction) {
      if (deploymentAction.dataset.action === 'cancel' && !confirm('Отменить это развёртывание?')) return;
      deploymentAction.disabled = true;
      try { await runOperation(`/api/deployments/${encodeURIComponent(deploymentAction.dataset.id)}/${encodeURIComponent(deploymentAction.dataset.action)}`, {}, 'Действие с развёртыванием'); await loadDeployments(); }
      catch (error) { showNotice(error.message); }
      finally { deploymentAction.disabled = false; }
      return;
    }
    const closeDialog = event.target.closest('[data-close-dialog]');
    if (closeDialog) { document.getElementById(closeDialog.dataset.closeDialog).close(); return; }
    const promote = event.target.closest('.promote');
    if (promote) {
      const node = selectNode(promote.dataset.panel, promote.dataset.uuid); if (!node) return;
      state.selected = node; state.passwordAction = 'promote';
      $('#password-title').textContent = `Сделать актуальной: ${node.name}`; $('#password-description').textContent = 'Будет установлен deploy-key и выполнен полный типовой provisioning. Статус изменится только после всех проверок; пароль не сохраняется.'; $('#password-submit').textContent = 'Сделать актуальной'; $('#password').value = ''; passwordDialog.showModal(); setTimeout(() => $('#password').focus(), 50);
      return;
    }
    const actualize = event.target.closest('.actualize');
    if (actualize) {
      const node = selectNode(actualize.dataset.panel, actualize.dataset.uuid); if (!node) return;
      state.selected = node; state.passwordAction = 'actualize';
      if (node.managed) { actualize.disabled = true; try { await actualizeNode(node); } catch (error) { showNotice(error.message); } finally { actualize.disabled = false; } }
      else { $('#password-title').textContent = `Синхронизация: ${node.name}`; $('#password-description').textContent = 'Так как нода legacy, ввод пароля сначала полностью переведёт её под управление, затем синхронизирует настройки. Пароль не сохраняется.'; $('#password-submit').textContent = 'Сделать актуальной и синхронизировать'; $('#password').value = ''; passwordDialog.showModal(); setTimeout(() => $('#password').focus(), 50); }
      return;
    }
    const connect = event.target.closest('.connect');
    if (connect) {
      const node = selectNode(connect.dataset.panel, connect.dataset.uuid); if (!node) return;
      state.selected = node; state.passwordAction = 'ssh';
      if (node.managed) { connect.disabled = true; try { await createTicket(node); } catch (error) { showNotice(error.message); } finally { connect.disabled = false; } }
      else { $('#password-title').textContent = `SSH: ${node.name}`; $('#password-description').textContent = 'Перед подключением нода будет полностью переведена под управление. Затем SSH откроется по deploy-key; пароль не сохраняется.'; $('#password-submit').textContent = 'Сделать актуальной и подключиться'; $('#password').value = ''; passwordDialog.showModal(); setTimeout(() => $('#password').focus(), 50); }
    }
    if (event.target.matches('[data-close]')) { $('#password').value = ''; passwordDialog.close(); }
  });
  $('#password-form').addEventListener('submit', async (event) => { event.preventDefault(); const password = $('#password').value; $('#password').value = ''; passwordDialog.close(); try { if (state.passwordAction === 'actualize' || state.passwordAction === 'promote') await actualizeNode(state.selected, password); else await createTicket(state.selected, password); } catch (error) { showNotice(error.message); } });
  $('#add-node-form').addEventListener('submit', async (event) => {
    event.preventDefault(); const dialog = $('#add-node-dialog');
    const password = $('#node-password').value; $('#node-password').value = ''; dialog.close();
    try { await runOperation('/api/deployments/standard', { panelId: $('#node-panel').value, hostId: $('#node-host').value, nodeName: $('#node-name').value.trim(), vpsIp: $('#node-ip').value.trim(), password }, `Установка ${$('#node-name').value.trim()}`); }
    catch (error) { showNotice(error.message); }
  });
  $('#add-balancer-form').addEventListener('submit', async (event) => {
    event.preventDefault(); const dialog = $('#add-balancer-dialog');
    const password = $('#balancer-password').value; $('#balancer-password').value = ''; dialog.close();
    try { await runOperation('/api/deployments/balancer', { panelId: $('#balancer-panel').value, internalSquadId: $('#balancer-squad').value, balancerTag: $('#balancer-tag').value, hostFqdn: $('#balancer-fqdn').value.trim(), nodeName: $('#balancer-name').value.trim(), ingressIp: $('#balancer-ingress').value.trim(), egressIp: $('#balancer-egress').value.trim(), password }, `Создание балансировщика ${$('#balancer-name').value.trim()}`); }
    catch (error) { showNotice(error.message); }
  });
  $('#node-operation-form').addEventListener('submit', async (event) => {
    event.preventDefault(); const node = state.selected; if (!node) return;
    let body = {}; let title = ''; let operationPath = state.nodeOperation;
    if (state.nodeOperation === 'ip') {
      const provider = $('#operation-ip-mode').value;
      body = { newIp: $('#operation-new-ip').value.trim() };
      if (provider !== 'panel') { body.provider = provider; body.password = $('#operation-ip-password').value; $('#operation-ip-password').value = ''; operationPath = 'server-ip'; }
      title = `Смена IP ${node.name}`;
    }
    else if (state.nodeOperation === 'move') { body = { hostId: $('#operation-host').value, password: $('#operation-password').value }; $('#operation-password').value = ''; title = `Перенос ${node.name}`; }
    else { title = `DNS ${node.name}`; }
    $('#node-operation-dialog').close();
    try { await runOperation(`/api/nodes/${encodeURIComponent(node.panelId)}/${encodeURIComponent(node.uuid)}/${operationPath}`, body, title); }
    catch (error) { showNotice(error.message); }
  });
  $('#bulk-ip-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    try {
      const payload = bulkIPPayload();
      if (!state.bulkIPPayload || JSON.stringify(state.bulkIPPayload) !== JSON.stringify(payload)) {
        const plan = await request('/api/nodes/ip-bulk/preview', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
        state.bulkIPPayload = payload;
        $('#bulk-ip-preview').innerHTML = `<strong>План: ${plan.items.length} нод</strong><p class="muted">Disabled: ${plan.disabledCount}, без общей DNS-зоны: ${plan.withoutDns}</p>${plan.items.map((item) => `<div class="deployment-log"><strong>${escapeHTML(item.nodeName)}</strong>: ${escapeHTML(item.oldIp)} → ${escapeHTML(item.newIp)}${item.wasDisabled ? ' · disabled' : ''}${(item.dnsZones || []).length ? ` · DNS: ${escapeHTML(item.dnsZones.join(', '))}` : ' · собственный Host/без DNS'}</div>`).join('')}`;
        $('#bulk-ip-preview').classList.remove('hidden');
        $('#bulk-ip-form button[type="submit"]').textContent = 'Применить план';
        return;
      }
      $('#bulk-ip-dialog').close();
      await runOperation('/api/nodes/ip-bulk/apply', payload, 'Массовая смена IP');
      state.bulkIPPayload = null;
    } catch (error) { showNotice(error.message); }
  });
  $('#terminal-close').addEventListener('click', closeTerminal);
  terminalDialog.addEventListener('cancel', (event) => { event.preventDefault(); closeTerminal(); });
  $('#refresh').addEventListener('click', loadNodes);
  $('#actualize-managed').addEventListener('click', actualizeManagedNodes);
  $('#add-node').addEventListener('click', openAddNode);
  $('#add-balancer').addEventListener('click', openAddBalancer);
  $('#bulk-ip').addEventListener('click', openBulkIP);
  $('#deployments-button').addEventListener('click', openDeployments);
  $('#node-panel').addEventListener('change', (event) => loadHosts(event.target.value).catch((error) => showNotice(error.message)));
  $('#balancer-panel').addEventListener('change', (event) => loadBalancerOptions(event.target.value).catch((error) => showNotice(error.message)));
  $('#search').addEventListener('input', (event) => { state.query = event.target.value; render(); });
  document.querySelectorAll('.filter').forEach((button) => button.addEventListener('click', () => { document.querySelectorAll('.filter').forEach((item) => item.classList.remove('active')); button.classList.add('active'); state.filter = button.dataset.filter; render(); }));
  $('#logout').addEventListener('click', async () => { try { await request('/logout', { method: 'POST' }); location.href = '/login'; } catch (error) { showNotice(error.message); } });

  (async () => { try { const session = await request('/api/session'); state.csrf = session.csrfToken; $('#operator').textContent = session.user.name || `Telegram ${session.user.id}`; await Promise.all([loadNodes(), loadCapabilities()]); } catch (error) { showNotice(error.message); } })();
})();
