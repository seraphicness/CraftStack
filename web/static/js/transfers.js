/* Folder transfers retain relative paths; chunks and receipts allow retry after
   connection loss. A reload requires selecting the same local files again. */
window.CSTransfers = (() => {
  const jobs = []; let running = false;
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
        j.state='완료';localStorage.removeItem(j.key);await j.refresh();
      }catch(e){j.state=j.cancel?'취소':'실패';j.error=e.message;}
      panel();
    }} finally{running=false;}
  }
  async function enqueue(api,dir,files,refresh,emptyDirs=[]) {
    const items=Array.from(files).filter(Boolean).map(f=>({file:f.file||f,path:join(dir,f.path||f.webkitRelativePath||f.name)}));
    const cache=new Map(),conflicts=[];
    for(const i of items){const parent=i.path.split('/').slice(0,-1).join('/');if(!cache.has(parent)){try{const d=await request(api+'/files?path='+encodeURIComponent(parent));cache.set(parent,new Set((d.entries||[]).map(e=>e.name)));}catch{cache.set(parent,new Set());}}if(cache.get(parent).has(i.path.split('/').pop()))conflicts.push(i.path);}
    const modal=document.createElement('dialog');modal.style.cssText='background:#252526;color:white;padding:24px;max-width:600px';
    const text=document.createElement('p');text.textContent=`파일 ${items.length}개 / ${items.reduce((n,i)=>n+i.file.size,0).toLocaleString()} bytes / 빈 폴더 ${emptyDirs.length}개 / 충돌 ${conflicts.length}개`;modal.append(text);
    const list=document.createElement('pre');list.style.maxHeight='180px';list.style.overflow='auto';list.textContent=conflicts.join('\n');modal.append(list);
    const select=document.createElement('select');for(const [v,t]of [['overwrite','덮어쓰기'],['skip','건너뛰기'],['rename','이름 변경']]){const o=document.createElement('option');o.value=v;o.textContent=t;select.append(o);}modal.append(select);
    const ok=document.createElement('button');ok.textContent='업로드';const cancel=document.createElement('button');cancel.textContent='취소';modal.append(ok,cancel);document.body.append(modal);modal.showModal();
    const accepted=await new Promise(resolve=>{ok.onclick=()=>resolve(true);cancel.onclick=()=>resolve(false);modal.oncancel=()=>resolve(false);});const conflict=select.value;modal.remove();if(!accepted)return;
    for(const p of emptyDirs)await request(api+'/files/mkdir',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:join(dir,p)})});
    for(const item of items){const key='craftstack-upload:'+JSON.stringify([api,item.path,item.file.size,item.file.lastModified,conflict]);const id=localStorage.getItem(key)||crypto.randomUUID();localStorage.setItem(key,id);jobs.push({...item,key,id,api,refresh,conflict,state:'대기',offset:0});}
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
