const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { runInNewContext } = require('node:vm');
const { webcrypto } = require('node:crypto');

function browser({ blockedStorage = false, cancel = false, policy = 'overwrite', failOnce = false } = {}) {
  const calls = [], values = new Map();
  let failures = failOnce ? 1 : 0;
  class Element {
    constructor(tag) { this.tag = tag; this.children = []; this.style = {}; this.value = ''; }
    append(...children) { this.children.push(...children); }
    replaceChildren() { this.children = []; }
    remove() {}
    showModal() {
      setImmediate(() => {
        const nodes = this.descendants();
        nodes.find(n => n.tag === 'select').value = policy;
        nodes.find(n => n.textContent === (cancel ? '취소' : '업로드 시작')).onclick();
      });
    }
    descendants() { return this.children.flatMap(n => [n, ...n.descendants()]); }
  }
  const body = new Element('body');
  const localStorage = Object.fromEntries(['getItem', 'setItem', 'removeItem'].map(method => [method, (key, value) => {
    if (blockedStorage) throw Error('Storage denied');
    if (method === 'getItem') return values.get(key);
    if (method === 'setItem') values.set(key, value);
    if (method === 'removeItem') values.delete(key);
  }]));
  class XHR {
    constructor() { this.upload = {}; }
    open(method, url) { this.url = url; }
    setRequestHeader() {}
    send(blob) {
      calls.push({ type: 'chunk', url: this.url, size: blob.size });
      if (failures-- > 0) { this.onerror(); return; }
      const params = new URL(this.url, 'http://server').searchParams;
      const offset = Number(params.get('offset')) + blob.size;
      this.status = 200;
      this.responseText = JSON.stringify({ offset, complete: offset === Number(params.get('total')) });
      this.onload();
    }
  }
  const window = {};
  runInNewContext(readFileSync(require.resolve('./static/js/transfers.js'), 'utf8'), {
    window, document: { body, createElement: tag => new Element(tag), getElementById: id => body.children.find(n => n.id === id) },
    // Match HTTP on a non-loopback host: randomUUID is intentionally absent.
    crypto: { getRandomValues: bytes => webcrypto.getRandomValues(bytes) },
    localStorage, URLSearchParams, XMLHttpRequest: XHR,
    fetch: async url => {
      calls.push({ type: 'fetch', url });
      return { ok: true, json: async () => url.includes('/chunks?') ? { offset: 0, complete: false } : { entries: [{ name: 'server.jar' }] } };
    }
  });
  return { transfers: window.CSTransfers, calls, body };
}
function file() { return { name: 'server.jar', size: 1048581, lastModified: 123, slice: (start, end) => ({ size: Math.min(end, 1048581) - start }) }; }

for (const policy of ['overwrite', 'skip', 'rename']) {
  test(`HTTP upload starts and sends chunks with ${policy} policy`, async () => {
    const b = browser({ policy });
    await b.transfers.enqueue('/api/instances/one', '', [file()], async () => {});
    const chunks = b.calls.filter(c => c.type === 'chunk');
    assert.equal(chunks.length, 2);
    const params = new URL(chunks[0].url, 'http://server').searchParams;
    assert.equal(params.get('conflict'), policy);
    assert.match(params.get('upload_id'), /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    assert.equal(chunks.reduce((sum, c) => sum + c.size, 0), file().size);
  });
}
test('disabled storage does not prevent upload; retries keep the upload ID', async () => {
  const b = browser({ blockedStorage: true, failOnce: true });
  await b.transfers.enqueue('/api/instances/one', '', [file()], async () => {});
  await b.transfers.enqueue('/api/instances/one', '', [file()], async () => {});
  const chunks = b.calls.filter(c => c.type === 'chunk');
  assert.equal(chunks.length, 3);
  const ids = chunks.map(c => new URL(c.url, 'http://server').searchParams.get('upload_id'));
  assert.equal(new Set(ids).size, 1);
});
test('cancelled confirmation never starts an upload', async () => {
  const b = browser({ cancel: true });
  await b.transfers.enqueue('/api/instances/one', '', [file()], async () => {});
  assert.equal(b.calls.some(c => c.url.includes('/chunks?')), false);
});
