// Chat panel: posts one message and streams the reply as server-sent events.
// Agent text reaches the DOM only through textContent.
(function () {
  'use strict';
  var panel = document.getElementById('chat');
  if (!panel) return;
  var url = panel.dataset.chatUrl, csrf = panel.dataset.csrf, contextId = panel.dataset.contextId;
  var log = document.getElementById('log'), msg = document.getElementById('msg');
  var sendBtn = document.getElementById('send'), stopBtn = document.getElementById('stop');

  var ctrl = null;          // AbortController of the running reply
  var open = null;          // progress line still receiving partial chunks
  var lastProgress = null;  // most recent progress line
  var answered = false, errored = false, gotDone = false;

  function line(cls, text) {
    var d = document.createElement('div');
    d.className = cls;
    d.textContent = text;
    log.appendChild(d);
    log.scrollTop = log.scrollHeight;
    return d;
  }

  function busy(on) {
    msg.disabled = on;
    sendBtn.disabled = on;
    stopBtn.disabled = !on;
    if (!on) msg.focus();
  }

  function dur(ms) {
    if (ms < 1000) return ms + ' ms';
    var s = Math.round(ms / 1000);
    return s < 60 ? s + 's' : Math.floor(s / 60) + 'm' + (s % 60) + 's';
  }

  function diag(d) {
    var parts = ['first event ' + d.first_event_ms + ' ms', d.events + ' events', dur(d.total_ms), d.state || 'no state'];
    if (d.task_id) parts.push('task ' + d.task_id.slice(0, 8) + '…');
    return parts.join(' · ');
  }

  function onEvent(type, d) {
    switch (type) {
      case 'progress':
        if (d.partial) {
          if (open) open.textContent += d.text;
          else open = lastProgress = line('progress', d.text);
        } else if (open) {
          // A complete step repeats its partial chunks: replace, don't add.
          open.textContent = d.text;
          lastProgress = open;
          open = null;
        } else {
          lastProgress = line('progress', d.text);
        }
        break;
      case 'tool':
        open = null;
        line('tool', 'tool: ' + d.name);
        break;
      case 'answer':
        // The artifact usually repeats the last step: show it once, as the answer.
        if (lastProgress && lastProgress.textContent === d.text) lastProgress.remove();
        open = lastProgress = null;
        answered = true;
        line('agent', d.text);
        break;
      case 'error':
        errored = true;
        open = null;
        line('err', d.message);
        break;
      case 'done':
        gotDone = true;
        if (!answered && !errored && lastProgress) lastProgress.className = 'agent';
        line('diag', diag(d));
        break;
    }
  }

  // SSE parser that tolerates chunks split mid-line.
  function parser() {
    var buf = '', type = '', data = [];
    return function (chunk) {
      buf += chunk;
      var i;
      while ((i = buf.indexOf('\n')) >= 0) {
        var ln = buf.slice(0, i);
        buf = buf.slice(i + 1);
        if (ln.slice(-1) === '\r') ln = ln.slice(0, -1);
        if (ln === '') {
          if (type && data.length) {
            try { onEvent(type, JSON.parse(data.join('\n'))); } catch (e) { /* ignore a bad event */ }
          }
          type = '';
          data = [];
        } else if (ln[0] === ':') {
          // comment (heartbeat)
        } else if (ln.indexOf('event:') === 0) {
          type = ln.slice(6).trim();
        } else if (ln.indexOf('data:') === 0) {
          data.push(ln.slice(5).replace(/^ /, ''));
        }
      }
    };
  }

  function httpError(code) {
    if (code === 401) {
      var d = line('err', 'Session expired: ');
      var a = document.createElement('a');
      a.href = '/';
      a.textContent = 'log in again';
      d.appendChild(a);
      return;
    }
    var text = {
      403: 'Page expired: reload', 404: 'Cluster no longer available: reload',
      409: 'Cluster no longer available: reload', 413: 'Message too long',
      429: 'A reply is already running in another tab'
    }[code];
    line('err', text || 'Request failed (HTTP ' + code + ')');
  }

  async function send() {
    var text = msg.value.trim();
    if (!text || ctrl) return;
    line('you', text);
    msg.value = '';
    open = lastProgress = null;
    answered = errored = gotDone = false;
    ctrl = new AbortController();
    var signal = ctrl.signal;
    busy(true);
    try {
      var resp = await fetch(url, {
        method: 'POST', credentials: 'same-origin', signal: signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
        body: JSON.stringify({ text: text, context_id: contextId })
      });
      if (resp.status !== 200) {
        msg.value = text;
        httpError(resp.status);
        return;
      }
      var reader = resp.body.getReader(), dec = new TextDecoder(), feed = parser();
      for (;;) {
        var r = await reader.read();
        if (r.done) break;
        feed(dec.decode(r.value, { stream: true }));
      }
      feed(dec.decode());
      if (!gotDone) line('err', 'The connection to the portal was cut before the reply finished');
    } catch (e) {
      line('err', signal.aborted ? 'Stopped' : 'Request failed');
    } finally {
      ctrl = null;
      busy(false);
    }
  }

  sendBtn.addEventListener('click', send);
  stopBtn.addEventListener('click', function () { if (ctrl) ctrl.abort(); });
  msg.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      send();
    }
  });
})();
