'use strict';
// Small DOM-only Markdown renderer. parse() is pure; render() builds nodes with
// createElement/createTextNode only, so agent text can never become markup.
(function () {
  var MAX_DEPTH = 6;
  var URL_RE = /^https?:\/\//i;
  var FENCE = '```';

  function text(v) { return { t: 'text', v: v }; }

  function isAlnum(ch) { return ch !== undefined && ch !== '' && /[A-Za-z0-9]/.test(ch); }

  // pair[i] is the index of the ')' matching the '(' at i, or -1. One stack pass.
  function pairParens(s) {
    var pair = new Array(s.length);
    var stack = [];
    for (var i = 0; i < s.length; i++) {
      pair[i] = -1;
      if (s[i] === '(') stack.push(i);
      else if (s[i] === ')' && stack.length) pair[stack.pop()] = i;
    }
    return pair;
  }

  function pushAll(dst, src) {
    for (var i = 0; i < src.length; i++) dst.push(src[i]);
  }

  function inline(s, depth) {
    var out = [];
    var buf = '';
    var i = 0;
    function flush() { if (buf) { out.push(text(buf)); buf = ''; } }
    // Memoised closer search. A closer's validity depends only on its position,
    // so a result found from an earlier start is reusable by later starts; this
    // keeps adversarial lines (thousands of unclosed openers) linear.
    var memo = {};
    var pair = null; // computed on first link candidate
    function find(delim, from) {
      var m = memo[delim];
      if (m && from >= m.from && (m.pos < 0 || m.pos >= from)) return m.pos;
      var pos = from;
      for (;;) {
        pos = s.indexOf(delim, pos);
        if (pos < 0 || delim !== '_' || !isAlnum(s[pos + 1])) break;
        pos++;
      }
      memo[delim] = { from: from, pos: pos };
      return pos;
    }
    while (i < s.length) {
      var ch = s[i];
      var end;
      if (ch === '`') {
        end = find('`', i + 1);
        if (end > i + 1) { flush(); out.push({ t: 'code', v: s.slice(i + 1, end) }); i = end + 1; continue; }
      } else if (ch === '*' && s[i + 1] === '*' && depth < MAX_DEPTH) {
        end = find('**', i + 2);
        if (end > i + 2) {
          while (s[end + 2] === '*') end++; // closing run: **a *b***
          flush(); out.push({ t: 'b', c: inline(s.slice(i + 2, end), depth + 1) }); i = end + 2; continue;
        }
      } else if ((ch === '*' || ch === '_') && depth < MAX_DEPTH && s[i + 1] !== ' ' && s[i + 1] !== ch &&
                 !(ch === '_' && isAlnum(s[i - 1]))) {
        end = find(ch, i + 1);
        if (end > i + 1 && s[end - 1] !== ' ') {
          flush(); out.push({ t: 'i', c: inline(s.slice(i + 1, end), depth + 1) }); i = end + 1; continue;
        }
      } else if (ch === '[' && depth < MAX_DEPTH) {
        var close = find(']', i + 1);
        if (close > i && s[close + 1] === '(') {
          if (!pair) pair = pairParens(s);
          var pe = pair[close + 1];
          if (pe > 0 && pe - close < 2048) { // longer URLs are not links
            var label = inline(s.slice(i + 1, close), depth + 1);
            var href = s.slice(close + 2, pe).trim();
            flush();
            if (URL_RE.test(href)) out.push({ t: 'a', href: href, c: label });
            else pushAll(out, label);
            i = pe + 1; continue;
          }
        }
      }
      buf += ch;
      i++;
    }
    flush();
    return out;
  }

  // Lines joined with <br>.
  function inlineLines(lines) {
    var out = [];
    for (var i = 0; i < lines.length; i++) {
      if (i) out.push({ t: 'br' });
      pushAll(out, inline(lines[i], 0));
    }
    return out;
  }

  function heading(line) {
    var n = 0;
    while (n < line.length && line[n] === '#') n++;
    if (n < 1 || n > 3 || line[n] !== ' ') return null;
    return { t: 'h' + n, c: inline(line.slice(n + 1).trim(), 0) };
  }

  function listItem(line) {
    var ind = 0;
    while (line[ind] === ' ') ind++;
    var rest = line.slice(ind);
    var m = 0;
    if ((rest[0] === '-' || rest[0] === '*' || rest[0] === '+') && rest[1] === ' ') m = 1;
    else {
      while (m < rest.length && rest[m] >= '0' && rest[m] <= '9') m++;
      if (m === 0 || rest[m] !== '.' || rest[m + 1] !== ' ') return null;
      m++;
      return { indent: ind, ordered: true, body: rest.slice(m + 1).trim() };
    }
    return { indent: ind, ordered: false, body: rest.slice(2).trim() };
  }

  function splitRow(line) {
    var s = line.trim();
    if (s[0] === '|') s = s.slice(1);
    if (s[s.length - 1] === '|') s = s.slice(0, -1);
    return s.split('|').map(function (c) { return c.trim(); });
  }

  function isSeparator(line) {
    if (line === undefined || line.indexOf('|') < 0) return false;
    var cells = splitRow(line);
    for (var i = 0; i < cells.length; i++) {
      var c = cells[i];
      if (c[0] === ':') c = c.slice(1);
      if (c[c.length - 1] === ':') c = c.slice(0, -1);
      if (!c) return false;
      for (var j = 0; j < c.length; j++) if (c[j] !== '-') return false;
    }
    return true;
  }

  function startsTable(lines, i) {
    return lines[i].indexOf('|') >= 0 && isSeparator(lines[i + 1]);
  }

  function startsBlock(lines, i) {
    var l = lines[i];
    return l.trim().indexOf(FENCE) === 0 || heading(l) !== null || l[0] === '>' ||
      listItem(l) !== null || startsTable(lines, i);
  }

  function parse(src) {
    var lines = String(src).replace(/\r\n?/g, '\n').split('\n');
    var blocks = [];
    var i = 0;
    while (i < lines.length) {
      var line = lines[i];
      if (!line.trim()) { i++; continue; }

      if (line.trim().indexOf(FENCE) === 0) {
        var lang = line.trim().slice(3).trim().split(' ')[0];
        var body = [];
        i++;
        while (i < lines.length && lines[i].trim().indexOf(FENCE) !== 0) body.push(lines[i++]);
        i++; // closing fence (or past the end when unclosed)
        blocks.push({ t: 'code', lang: lang, text: body.join('\n') });
        continue;
      }

      var h = heading(line);
      if (h) { blocks.push(h); i++; continue; }

      if (line[0] === '>') {
        var q = [];
        while (i < lines.length && lines[i][0] === '>') q.push(lines[i++].slice(1).replace(/^ /, ''));
        blocks.push({ t: 'quote', c: inlineLines(q) });
        continue;
      }

      var li = listItem(line);
      if (li) {
        var list = { t: li.ordered ? 'ol' : 'ul', items: [] };
        var cur = null;
        while (i < lines.length && (li = listItem(lines[i])) !== null) {
          if (li.indent >= 2 && cur) {
            cur.sub = cur.sub || [];
            cur.sub.push(inline(li.body, 0));
          } else {
            cur = inline(li.body, 0);
            list.items.push(cur);
          }
          i++;
        }
        blocks.push(list);
        continue;
      }

      if (startsTable(lines, i)) {
        var tbl = { t: 'table', head: splitRow(line).map(function (c) { return inline(c, 0); }), rows: [] };
        i += 2;
        while (i < lines.length && lines[i].trim() && lines[i].indexOf('|') >= 0) {
          tbl.rows.push(splitRow(lines[i++]).map(function (c) { return inline(c, 0); }));
        }
        blocks.push(tbl);
        continue;
      }

      var para = [line];
      i++;
      while (i < lines.length && lines[i].trim() && !startsBlock(lines, i)) para.push(lines[i++]);
      blocks.push({ t: 'p', c: inlineLines(para) });
    }
    return blocks;
  }

  var TAGS = { b: 'strong', i: 'em', code: 'code' };

  function renderInline(nodes, parent, doc) {
    nodes.forEach(function (n) {
      var el;
      if (n.t === 'text') { parent.appendChild(doc.createTextNode(n.v)); return; }
      if (n.t === 'br') { parent.appendChild(doc.createElement('br')); return; }
      if (n.t === 'a') {
        el = doc.createElement('a');
        el.setAttribute('href', n.href);
        el.setAttribute('target', '_blank');
        el.setAttribute('rel', 'noopener noreferrer');
        renderInline(n.c, el, doc);
      } else if (n.t === 'code') {
        el = doc.createElement('code');
        el.textContent = n.v;
      } else {
        el = doc.createElement(TAGS[n.t]);
        renderInline(n.c, el, doc);
      }
      parent.appendChild(el);
    });
  }

  function cell(tag, inl, doc) {
    var el = doc.createElement(tag);
    renderInline(inl, el, doc);
    return el;
  }

  function copyButton(code, doc) {
    var btn = doc.createElement('button');
    btn.className = 'md-copy';
    btn.setAttribute('type', 'button');
    btn.textContent = 'Copy';
    btn.addEventListener('click', function () {
      if (typeof navigator === 'undefined' || !navigator.clipboard) return;
      navigator.clipboard.writeText(code).then(function () {
        btn.textContent = 'Copied';
        setTimeout(function () { btn.textContent = 'Copy'; }, 1500);
      }, function () {});
    });
    return btn;
  }

  function render(src, doc) {
    doc = doc || document;
    var frag = doc.createDocumentFragment();
    parse(src).forEach(function (b) {
      var el;
      if (b.t === 'code') {
        el = doc.createElement('div');
        el.className = 'md-code';
        var pre = doc.createElement('pre');
        var code = doc.createElement('code');
        code.textContent = b.text;
        pre.appendChild(code);
        el.appendChild(copyButton(b.text, doc));
        el.appendChild(pre);
      } else if (b.t === 'ul' || b.t === 'ol') {
        el = doc.createElement(b.t);
        b.items.forEach(function (item) {
          var li = doc.createElement('li');
          renderInline(item, li, doc);
          if (item.sub) {
            var sub = doc.createElement(b.t);
            item.sub.forEach(function (s) {
              var sli = doc.createElement('li');
              renderInline(s, sli, doc);
              sub.appendChild(sli);
            });
            li.appendChild(sub);
          }
          el.appendChild(li);
        });
      } else if (b.t === 'table') {
        el = doc.createElement('table');
        var thead = doc.createElement('thead');
        var hr = doc.createElement('tr');
        b.head.forEach(function (c) { hr.appendChild(cell('th', c, doc)); });
        thead.appendChild(hr);
        el.appendChild(thead);
        var tbody = doc.createElement('tbody');
        b.rows.forEach(function (r) {
          var tr = doc.createElement('tr');
          r.forEach(function (c) { tr.appendChild(cell('td', c, doc)); });
          tbody.appendChild(tr);
        });
        el.appendChild(tbody);
      } else {
        el = doc.createElement(b.t === 'quote' ? 'blockquote' : b.t);
        renderInline(b.c, el, doc);
      }
      frag.appendChild(el);
    });
    return frag;
  }

  var api = { parse: parse, render: render };
  if (typeof module !== 'undefined') module.exports = api; else window.Markdown = api;
})();
