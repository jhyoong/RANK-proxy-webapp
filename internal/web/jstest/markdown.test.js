'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const Markdown = require('../static/markdown.js');

const T = (v) => ({ t: 'text', v });

test('paragraphs split on blank lines; single newline is br', () => {
  assert.deepEqual(Markdown.parse('a\nb\n\nc'), [
    { t: 'p', c: [T('a'), { t: 'br' }, T('b')] },
    { t: 'p', c: [T('c')] },
  ]);
});

test('headings h1-h3; h4 is a paragraph', () => {
  const b = Markdown.parse('# a\n## b\n### c\n#### d');
  assert.deepEqual(b.map((x) => x.t), ['h1', 'h2', 'h3', 'p']);
  assert.deepEqual(b[0].c, [T('a')]);
  assert.deepEqual(b[3].c, [T('#### d')]);
});

test('inline emphasis and code', () => {
  const c = (s) => Markdown.parse(s)[0].c;
  assert.deepEqual(c('**b**'), [{ t: 'b', c: [T('b')] }]);
  assert.deepEqual(c('*i*'), [{ t: 'i', c: [T('i')] }]);
  assert.deepEqual(c('_i_'), [{ t: 'i', c: [T('i')] }]);
  assert.deepEqual(c('`c`'), [{ t: 'code', v: 'c' }]);
  assert.deepEqual(c('**a *b***'), [{ t: 'b', c: [T('a '), { t: 'i', c: [T('b')] }] }]);
  assert.deepEqual(c('snake_case_name'), [T('snake_case_name')]);
});

test('fenced code keeps text literally', () => {
  const b = Markdown.parse('```html\n<script>alert(1)</script>\n  x\n```\nafter');
  assert.deepEqual(b[0], { t: 'code', lang: 'html', text: '<script>alert(1)</script>\n  x' });
  assert.equal(b[1].t, 'p');
});

test('unclosed fence runs to the end', () => {
  const b = Markdown.parse('```\nkubectl get pods\nNAME');
  assert.deepEqual(b, [{ t: 'code', lang: '', text: 'kubectl get pods\nNAME' }]);
});

test('lists with one nesting level', () => {
  const ul = Markdown.parse('- a\n* b\n  - sub')[0];
  assert.equal(ul.t, 'ul');
  assert.equal(ul.items.length, 2);
  assert.deepEqual(ul.items[0], [T('a')]);
  assert.deepEqual(ul.items[1].sub, [[T('sub')]]);
  const ol = Markdown.parse('1. c\n2. d')[0];
  assert.equal(ol.t, 'ol');
  assert.equal(ol.items.length, 2);
});

test('pipe tables', () => {
  const b = Markdown.parse('| A | B |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |');
  assert.deepEqual(b, [{
    t: 'table',
    head: [[T('A')], [T('B')]],
    rows: [[[T('1')], [T('2')]], [[T('3')], [T('4')]]],
  }]);
  const p = Markdown.parse('a | b\nc | d');
  assert.equal(p.length, 1);
  assert.equal(p[0].t, 'p');
});

test('links keep only http(s) hrefs', () => {
  assert.deepEqual(Markdown.parse('[x](https://e.example)')[0].c, [{ t: 'a', href: 'https://e.example', c: [T('x')] }]);
  assert.deepEqual(Markdown.parse('[x](javascript:alert(1))')[0].c, [T('x')]);
  assert.deepEqual(Markdown.parse('[x](data:text/html,abc)')[0].c, [T('x')]);
});

test('raw HTML stays text', () => {
  assert.deepEqual(Markdown.parse('<img src=x onerror=alert(1)>')[0].c, [T('<img src=x onerror=alert(1)>')]);
});

test('blockquote', () => {
  assert.deepEqual(Markdown.parse('> q')[0], { t: 'quote', c: [T('q')] });
});

test('pathological input is linear', () => {
  const cases = ['_a ', '[](', '[a](', '[](((', '[a](b', '*a ', '**a ', '`a ', '[[a]', '[a](b(c'];
  for (const unit of cases) {
    for (const size of [60000, 240000]) {
      const n = Math.ceil(size / unit.length);
      const line = unit.repeat(n);
      const t0 = Date.now();
      Markdown.parse(line);
      const ms = Date.now() - t0;
      console.log(`  ${JSON.stringify(unit)} ${size / 1000} KB: ${ms} ms`);
      if (size === 240000) assert.ok(ms < 200, `${JSON.stringify(unit)} took ${ms} ms`);
    }
  }
  const t0 = Date.now();
  Markdown.parse('*'.repeat(20000) + '\n' + '['.repeat(20000) + '\n' + '`'.repeat(20000) + '\n' + '|'.repeat(20000));
  assert.ok(Date.now() - t0 < 200);
});

test('very many inline tokens do not overflow the stack', () => {
  assert.doesNotThrow(() => Markdown.parse('`a` '.repeat(200000)));
  assert.doesNotThrow(() => Markdown.parse('**a '.repeat(150000)));
  assert.doesNotThrow(() => Markdown.parse('[x](https://e.example) '.repeat(50000)));
});

function fakeDoc(log) {
  const mk = (name) => {
    const el = { name, children: [], attrs: {}, listeners: {}, className: '', textContent: '' };
    el.appendChild = (c) => { el.children.push(c); return c; };
    el.setAttribute = (k, v) => { el.attrs[k] = v; };
    el.addEventListener = (k, f) => { el.listeners[k] = f; };
    Object.defineProperty(el, 'innerHTML', { set() { throw new Error('innerHTML set'); }, get() { return ''; } });
    log.push(el);
    return el;
  };
  return {
    createElement: mk,
    createTextNode: (v) => ({ name: '#text', v }),
    createDocumentFragment: () => mk('#frag'),
  };
}

test('render builds nodes without innerHTML', () => {
  const log = [];
  const frag = Markdown.render('# h\n\n**b** [x](https://e.example) [y](javascript:1)\n\n```\ncode\n```\n\n| A |\n|---|\n| 1 |\n\n- a\n  - b', fakeDoc(log));
  assert.equal(frag.name, '#frag');
  const names = log.map((e) => e.name);
  for (const n of ['h1', 'p', 'strong', 'a', 'pre', 'code', 'button', 'table', 'ul']) assert.ok(names.includes(n), n);
  const a = log.find((e) => e.name === 'a');
  assert.equal(a.attrs.href, 'https://e.example');
  assert.equal(a.attrs.target, '_blank');
  assert.equal(a.attrs.rel, 'noopener noreferrer');
  assert.equal(log.filter((e) => e.name === 'a').length, 1);
  const btn = log.find((e) => e.name === 'button');
  let copied;
  // Node 21+ defines a read-only global navigator: swap it via a descriptor
  // and restore the original afterwards.
  const saved = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true, writable: true,
    value: { clipboard: { writeText: (s) => { copied = s; return Promise.resolve(); } } },
  });
  try {
    btn.listeners.click();
    assert.equal(copied, 'code');
  } finally {
    if (saved) Object.defineProperty(globalThis, 'navigator', saved);
    else delete globalThis.navigator;
  }
});

test('no static script uses HTML-injecting APIs', () => {
  const dir = path.join(__dirname, '..', 'static');
  const files = fs.readdirSync(dir).filter((f) => f.endsWith('.js'));
  assert.ok(files.includes('markdown.js'));
  for (const f of files) {
    const src = fs.readFileSync(path.join(dir, f), 'utf8');
    for (const bad of ['innerHTML', 'outerHTML', 'insertAdjacentHTML', 'document.write']) {
      assert.ok(!src.includes(bad), `${f} contains ${bad}`);
    }
  }
});
