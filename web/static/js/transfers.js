/* Folder transfers retain relative paths; chunks and receipts allow retry after
   connection loss. A reload requires selecting the same local files again. */
window.CSTransfers = (() => {
  const jobs = []; let running = false;
  // randomUUID is unavailable on ordinary HTTP origins; getRandomValues is not.
  function uploadID() {
    if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    bytes[6] = (bytes[6] & 15) | 64;
    bytes[8] = (bytes[8] & 63) | 128;
    const hex = Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
    return `${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
  }
  const receipts = new Map();
  function receipt(key) {
    try { return localStorage.getItem(key) || receipts.get(key); }
    catch { return receipts.get(key); }
  }
  function saveReceipt(key, id) {
    receipts.set(key, id);
    try { localStorage.setItem(key, id); } catch { /* Retry still works in this tab. */ }
  }
  function clearReceipt(key) {
    receipts.delete(key);
    try { localStorage.removeItem(key); } catch { /* Storage may be disabled. */ }
  }
  const join = (...p) => p.filter(Boolean).join('/').replace(/\/+/g, '/');
  async function request(url, options) {
    const r = await fetch(url, options), d = await r.json();
    if (!r.ok) throw Error(d.message || d.error || r.statusText);
    return d;
  }
  function panel() {
    let el = document.getElementById('transfer-queue');
    if (!el) { el = document.createElement('section'); el.id = 'transfer-queue'; el.style.cssText = 'position:fixed;bottom:30px;right:20px;z-index:400;background:#252526;color:white;padding:16px;max-height:55vh;overflow:auto;width:440px;max-width:90vw;border:1px solid #666'; document.body.append(el); }
    el.replaceChildren();
    const title = document.createElement('strong'); title.textContent = '업로드 작업'; el.append(title);
    for (const j of jobs) {
      const row = document.createElement('div'); row.style.marginTop = '10px';
      const label = document.createElement('div'); label.textContent = `${j.path} — ${j.state} ${j.offset}/${j.file.size} bytes`; row.append(label);
      const bar = document.createElement('progress'); bar.max = j.file.size || 1; bar.value = j.state === '완료' ? bar.max : j.offset; row.append(bar);
      const b = document.createElement('button'); b.textContent = j.state === '실패' || j.state === '취소' ? '재시도' : '취소'; b.style.marginLeft = '12px';
      b.disabled = j.state === '완료'; b.onclick = () => { if (j.state === '실패' || j.state === '취소') {j.cancel = false; j.state = '대기'; run();} else {j.cancel = true;j.xhr?.abort();j.state='취소';panel();} }; row.append(b);
      if (j.error) {const msg=document.createElement('div');msg.textContent=j.error;row.append(msg);} el.append(row);
    }
    const close=document.createElement('button');close.textContent='닫기';close.onclick=()=>el.remove();el.append(close);
  }
  function url(j, extra={}) {return j.api+'/files/chunks?'+new URLSearchParams({path:j.path,upload_id:j.id,offset:j.offset,total:j.file.size,conflict:j.conflict,...extra});}
  function send(j, blob) {return new Promise((resolve,reject)=>{
    const x=new XMLHttpRequest();j.xhr=x;x.open('POST',url(j));x.setRequestHeader('Content-Type','application/octet-stream');
    const offset=j.offset;x.upload.onprogress=e=>{j.offset=offset+e.loaded;panel();j.offset=offset;};
    x.onload=()=>{try{const d=JSON.parse(x.responseText);if(x.status>=400)throw Error(d.message||d.error);resolve(d);}catch(e){reject(e);}};
    x.onerror=()=>reject(Error('연결 실패 — 재시도하면 이어 올립니다'));x.onabort=()=>reject(Error('취소됨'));x.send(blob);
  });}
  async function run() {
    if(running)return;running=true;
    try {for(let j; (j=jobs.find(j=>j.state==='대기'&&!j.cancel));) {
      if(j.cancel)continue;j.state='전송 중';j.error='';panel();
      try {
        const status=await request(url(j,{status:'true'}),{method:'POST'});j.offset=status.offset||0;
        if(!status.complete) {do {if(j.cancel)throw Error('취소됨');const d=await send(j,j.file.slice(j.offset,j.offset+1024*1024));j.offset=d.offset||0;if(d.complete)break;}while(j.offset<j.file.size);}
        j.state='완료';clearReceipt(j.key);await j.refresh();
      }catch(e){j.state=j.cancel?'취소':'실패';j.error=e.message;}
      panel();
    }} finally{running=false;}
  }
  async function enqueue(api,dir,files,refresh,emptyDirs=[]) {
    const items=Array.from(files).filter(Boolean).map(f=>({file:f.file||f,path:join(dir,f.path||f.webkitRelativePath||f.name)}));
    if (!items.length && !emptyDirs.length) return;
    const cache=new Map(),conflicts=[];
    for(const i of items){const parent=i.path.split('/').slice(0,-1).join('/');if(!cache.has(parent)){try{const d=await request(api+'/files?path='+encodeURIComponent(parent));cache.set(parent,new Set((d.entries||[]).map(e=>e.name)));}catch{cache.set(parent,new Set());}}if(cache.get(parent).has(i.path.split('/').pop()))conflicts.push(i.path);}
    const modal=document.createElement('dialog');modal.style.cssText='background:#252526;color:white;padding:24px;width:540px;max-width:90vw;border:1px solid #666;border-radius:12px';
    const heading=document.createElement('h2');heading.textContent='업로드 확인';heading.style.cssText='font-size:20px;font-weight:bold;margin-bottom:12px';modal.append(heading);
    const text=document.createElement('p');text.textContent=`파일 ${items.length}개 / ${items.reduce((n,i)=>n+i.file.size,0).toLocaleString()} bytes / 빈 폴더 ${emptyDirs.length}개 / 충돌 ${conflicts.length}개`;modal.append(text);
    const list=document.createElement('pre');list.style.maxHeight='180px';list.style.overflow='auto';list.textContent=conflicts.join('\n');modal.append(list);
    const policy=document.createElement('label');policy.textContent='같은 이름의 파일이 있을 때';policy.style.cssText='display:block;margin:16px 0';
    const select=document.createElement('select');select.style.cssText='display:block;width:100%;margin-top:8px;padding:10px;background:#333;color:white;border:1px solid #888;border-radius:6px';for(const [v,t]of [['overwrite','덮어쓰기'],['skip','건너뛰기'],['rename','이름 변경']]){const o=document.createElement('option');o.value=v;o.textContent=t;select.append(o);}policy.append(select);modal.append(policy);
    const hint=document.createElement('p');hint.textContent='처리 방법을 선택한 뒤 아래 업로드 시작 버튼을 누르세요.';modal.append(hint);
    const actions=document.createElement('div');actions.style.cssText='display:flex;gap:12px;justify-content:flex-end;margin-top:20px';
    const ok=document.createElement('button');ok.type='button';ok.textContent='업로드 시작';ok.style.cssText='padding:10px 20px;background:#1674c4;color:white;border-radius:6px;font-weight:bold;cursor:pointer';
    const cancel=document.createElement('button');cancel.type='button';cancel.textContent='취소';cancel.style.cssText='padding:10px 20px;background:#444;color:white;border-radius:6px;cursor:pointer';actions.append(cancel,ok);modal.append(actions);document.body.append(modal);modal.showModal();
    const accepted=await new Promise(resolve=>{ok.onclick=()=>resolve(true);cancel.onclick=()=>resolve(false);modal.oncancel=()=>resolve(false);});const conflict=select.value;modal.remove();if(!accepted)return;
    for(const p of emptyDirs)await request(api+'/files/mkdir',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:join(dir,p)})});
    for(const item of items){const key='craftstack-upload:'+JSON.stringify([api,item.path,item.file.size,item.file.lastModified,conflict]);const id=receipt(key)||uploadID();saveReceipt(key,id);jobs.push({...item,key,id,api,refresh,conflict,state:'대기',offset:0});}
    panel();await run();await refresh();
  }
  async function drop(api,dir,dt,refresh) {
    const files=[],dirs=[];
    async function walk(entry,prefix='') {const path=join(prefix,entry.name);if(entry.isFile){files.push({file:await new Promise((ok,no)=>entry.file(ok,no)),path});return;}
      const reader=entry.createReader();let children=[];for(;;){const batch=await new Promise((ok,no)=>reader.readEntries(ok,no));if(!batch.length)break;children.push(...batch);}if(!children.length)dirs.push(path);for(const child of children)await walk(child,path);
    }
    // Capture entries synchronously before the DataTransfer becomes unavailable.
    const entries=Array.from(dt.items||[]).map(i=>i.webkitGetAsEntry?.()).filter(Boolean);const fallback=Array.from(dt.files||[]);
    if(entries.length){for(const e of entries)await walk(e);}else files.push(...fallback);
    return enqueue(api,dir,files,refresh,dirs);
  }
  return {enqueue,drop};
})();
