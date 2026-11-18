(() => {
  'use strict';
  const $ = (id) => document.getElementById(id);
  const escape = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  let candidates = [], current = null, busy = false, csrf = '';
  const statuses = { prepared:'Готово к запуску', blocked:'Пропущено', network_pending:'Проверка изменения сети / отката', network_done:'Сервер готов, завершается панель и DNS', control_pending:'Обновляются панель, DNS и основной IP', egress_pending:'Обновляется второй IP в Config Profile', done:'Выполнено', rolled_back:'Не применено / откат' };
  async function api(path, body) {
    const response = await fetch(path, body === undefined ? {} : {method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':csrf},body:JSON.stringify(body)});
    const result = await response.json(); if (!response.ok) throw new Error(result.error || 'Ошибка запроса'); return result;
  }
  function message(text) { $('leastload-status').textContent = text; }
  function setBusy(value) {
    busy=value;
    for (const id of ['leastload-panel','leastload-mode','leastload-preview','leastload-import','leastload-history-refresh']) $(id).disabled=value;
    $('leastload-apply').disabled=value || !current || !current.items.some((item)=>['prepared','network_pending','network_done','control_pending','egress_pending'].includes(item.status));
  }
  function renderCandidates() {
    const mode=$('leastload-mode').value,network=mode==='network',egress=mode==='egress';
    $('leastload-candidates').innerHTML=candidates.map((c,i)=>{const reason=c.reason||(network?c.networkReason:egress?c.egressReason:'');const old=egress?c.egressIp:c.address;return `<div class="leastload-row" data-search="${escape(`${c.name} ${c.address} ${c.egressIp||''}`.toLowerCase())}"><label><input type="checkbox" data-leastload-check="${i}" ${reason?'disabled':''}> ${escape(c.name)} · ${escape(egress?`второй IP ${old||'не задан'}`:c.address)}</label><input data-leastload-ip="${i}" aria-label="Новый ${egress?'второй ':''}IP для ${escape(c.name)}" placeholder="Новый IPv4" ${reason?'disabled':''}><small>${escape(reason || (egress?`основной ${c.address} · ${c.domain}`:c.domain))}</small></div>`;}).join('') || '<p>Ноды с отдельными скрытыми Host не найдены.</p>';
    filter();
  }
  function filter() { const q=$('leastload-search').value.toLowerCase();document.querySelectorAll('.leastload-row').forEach((row)=>{row.hidden=!row.dataset.search.includes(q);}); }
  async function loadCandidates() {current=null;setBusy(true);try{candidates=(await api(`/api/leastload/candidates?panel=${encodeURIComponent($('leastload-panel').value)}`)).candidates||[];renderCandidates();$('leastload-plan').textContent='';}finally{setBusy(false);} }
  function renderJob(job) {
    current=job;
    $('leastload-plan').innerHTML=`<p>Операция ${escape(job.id)} · ${escape(job.panelId)}</p>`+job.items.map((item)=>{
      const control=job.mode==='control_plane'||job.mode==='egress',others=(item.plan.before?.addresses||[]).filter((a)=>a.ip!==item.mapping.oldIp&&a.interface===item.plan.interface).map((a)=>`${a.ip}/${a.prefix}`);
      return `<article class="deployment-details"><strong>${escape(item.name||item.mapping.oldIp)}</strong><p>${escape(item.mapping.oldIp)} → ${escape(item.mapping.newIp)}</p><p>${escape(statuses[item.status]||item.status)}: ${escape(item.message)}</p>${item.status==='blocked'||control?'':`<p>Сохранить: ${escape(others.join(', ')||'дополнительных адресов не найдено')}${item.plan.mode!=='replace'?' · старый IP также сохраняется':''}</p>`}</article>`;
    }).join('');setBusy(busy);
  }
  async function history() {
    const jobs=(await api('/api/leastload/jobs')).jobs||[];
    $('leastload-history').innerHTML=jobs.map((job,i)=>`<button type="button" class="button secondary" data-leastload-history="${i}">${escape(new Date(job.createdAt).toLocaleString())} · ${escape(job.panelId)} · ${job.items.filter((item)=>item.status==='done').length}/${job.items.length}</button>`).join('')||'<p>Операций пока нет.</p>';
    $('leastload-history').onclick=(event)=>{const button=event.target.closest('[data-leastload-history]');if(button&&!busy){renderJob(jobs[Number(button.dataset.leastloadHistory)]);$('leastload-assigned').checked=false;}};
  }
  async function operation(path,body) {
    setBusy(true);message('Операция выполняется…');
    try {
      const {jobId}=await api(path,body);
      for (;;) {
        const job=await api(`/api/jobs/${encodeURIComponent(jobId)}`);message(job.progress?.message||'Операция выполняется…');
        if(job.status==='failed')throw new Error(job.error||'Операция не завершена. Откройте сохранённую операцию для проверки.');
        if(job.status==='completed'){renderJob(job.result);break;}
        await new Promise((resolve)=>setTimeout(resolve,2000));
      }
      await history();message('Результаты сохранены. Проверьте статус каждой ноды.');
    }catch(error){message(error.message);}finally{setBusy(false);}
  }
  $('leastload-open').onclick=async()=>{
    $('leastload-dialog').showModal();if(busy)return;try{csrf=(await api('/api/session')).csrfToken;const panels=(await api('/api/options/panels')).panels||[];$('leastload-panel').innerHTML=panels.map((p)=>`<option value="${escape(p.ID)}">${escape(p.Name)}</option>`).join('');await loadCandidates();await history();}catch(error){message(error.message);}
  };
  $('leastload-panel').onchange=()=>loadCandidates().catch((e)=>message(e.message));
  $('leastload-mode').onchange=()=>{const mode=$('leastload-mode').value;current=null;$('leastload-plan').textContent='';$('leastload-assigned').checked=false;$('leastload-confirm-text').textContent=mode==='control_plane'?'Понимаю: SSH и Netplan не проверяются, меняются только панель, DNS и основной IP':mode==='egress'?'Понимаю: без SSH меняется только второй IP в Xray sendThrough':'Новые IP уже назначены нужным серверам у хостера';renderCandidates();setBusy(false);};
  $('leastload-search').oninput=filter;
  $('leastload-history-refresh').onclick=()=>history().catch((e)=>message(e.message));
  $('leastload-import').onclick=()=>{
    try {
      const parsed=[],seen=new Set();
      for(const line of $('leastload-mappings').value.split('\n').map((v)=>v.trim()).filter(Boolean)){
        const parts=line.split(/->|→/).map((v)=>v.trim());if(parts.length!==2||!parts[1])throw new Error('Формат строки: старый_IP -> новый_IP');
        const egress=$('leastload-mode').value==='egress';const matches=candidates.map((c,i)=>({c,i})).filter(({c})=>(egress?c.egressIp:c.address)===parts[0]&&!c.reason&&!(egress&&c.egressReason));
        if(matches.length!==1||seen.has(parts[0]))throw new Error(`Неоднозначная, недоступная или повторная нода: ${parts[0]}`);
        seen.add(parts[0]);parsed.push({i:matches[0].i,ip:parts[1]});
      }
      document.querySelectorAll('[data-leastload-check]').forEach((el)=>{el.checked=false;});
      parsed.forEach(({i,ip})=>{document.querySelector(`[data-leastload-check="${i}"]`).checked=true;document.querySelector(`[data-leastload-ip="${i}"]`).value=ip;});message(`Выбрано нод: ${parsed.length}`);
    }catch(error){message(error.message);}
  };
  $('leastload-preview').onclick=()=>{
    const egress=$('leastload-mode').value==='egress';const mappings=[...document.querySelectorAll('[data-leastload-check]:checked')].map((el)=>{const i=Number(el.dataset.leastloadCheck),c=candidates[i];return {nodeUuid:c.nodeUuid,oldIp:egress?c.egressIp:c.address,newIp:document.querySelector(`[data-leastload-ip="${i}"]`).value.trim()};});
    if(!mappings.length||mappings.length>50){message('Выберите от 1 до 50 нод');return;}
    $('leastload-assigned').checked=false;operation('/api/leastload/preview',{panelId:$('leastload-panel').value,mode:$('leastload-mode').value,mappings});
  };
  $('leastload-apply').onclick=()=>{
    if(!current||!$('leastload-assigned').checked){message(current?.mode==='control_plane'||current?.mode==='egress'?'Подтвердите выполнение без проверки SSH и Netplan':'Подтвердите назначение новых IP серверам у хостера');return;}
    if(!confirm(current.mode==='control_plane'?'Изменить IP только в Remnawave, DNS и боте без проверки сервера?':current.mode==='egress'?'Изменить только второй IP в Xray sendThrough без подключения к серверу?':'Выполнить показанный план? Сеть изменится только на нодах, которым это требуется.'))return;
    operation('/api/leastload/apply',{jobId:current.id,assigned:true});
  };
})();
