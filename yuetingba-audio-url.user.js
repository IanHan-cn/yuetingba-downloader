// ==UserScript==
// @name         悦听吧 · 音频直链查看器
// @namespace    https://yuetingba.cn/
// @version      1.0.0
// @description  在播放页复现 audio-url / play-info 接口，显示并复制真实音频地址；也会自动捕获页面播放时的直链
// @author       WorkBuddy
// @match        *://yuetingba.cn/*
// @match        *://*.yuetingba.cn/*
// @grant        GM_setClipboard
// @grant        GM_registerMenuCommand
// @run-at       document-idle
// @noframes
// ==/UserScript==

/*
 * 对应命令行里的：
 *   curl -s '<origin>/api/app/docs-listen/<tingId>/audio-url' \
 *        -H 'Referer: <origin>/' \
 *        -H 'X-Ytb-Device: <localStorage: cn-docsite-user2-guid>' \
 *        -b '<浏览器 Cookie>'
 *
 * 在页面里跑时：
 *   - 相对路径 + credentials:'same-origin' 自动带上 Cookie 和页面 origin，不需要 Referer；
 *   - X-Ytb-Device 从 localStorage 读（页面自身的 ting-player 初始化时就写好了）；
 *   - 若站点换了域名/用了镜像，自行在 @match 里补一行即可。
 */

(function () {
  'use strict';

  var DEVICE_KEY = 'cn-docsite-user2-guid';
  var card = document.getElementById('tpCard');
  if (!card) return; // 只在播放页生效：靠 #tpCard[data-ting-id] 识别

  var audio = document.getElementById('tingAudio');

  function currentId() {
    return card.getAttribute('data-ting-id') || '';
  }

  function apiUrl(kind, id) {
    return '/api/app/docs-listen/' + encodeURIComponent(id) + '/' + kind;
  }

  function tingIdFrom(el) {
    if (!el) return '';
    var m = /\/book\/ting\/([0-9a-f-]{36})/i.exec(el.getAttribute('href') || '');
    return m ? m[1] : '';
  }

  function deviceId() {
    try { return localStorage.getItem(DEVICE_KEY) || ''; } catch (e) { return ''; }
  }

  /* ---------- 请求：等价于那条 curl ---------- */
  function request(path, wantJson) {
    var h = { Accept: wantJson ? 'application/json' : '*/*' };
    var did = deviceId();
    if (did) h['X-Ytb-Device'] = did;

    return fetch(path, {
      method: 'GET',
      credentials: 'same-origin', // 自动带浏览器 Cookie
      headers: h,
      cache: 'no-store'
    }).then(function (res) {
      if (res.status === 429) {
        return res.json().catch(function () { return {}; }).then(function (info) {
          var sec = Number(info.retryAfter) || 30;
          var extra = info.limit ? '（上限 ' + info.limit + ' 次 / ' + (info.window || '?') + ' 秒）' : '';
          throw new Error('被限流 429：' + sec + ' 秒后再试' + extra);
        });
      }
      if (!res.ok) throw new Error('HTTP ' + res.status + ' ' + res.statusText);
      return wantJson ? res.json() : res.text();
    });
  }

  // 接口正常情况下返回纯文本 URL；兼容页面自身也会处理的 JSON 字符串 / 对象形式
  function normalizeUrl(text) {
    var s = String(text || '').trim();
    if (s.charAt(0) === '"' || s.charAt(0) === '{') {
      try {
        var j = JSON.parse(s);
        s = typeof j === 'string' ? j : (j.audioUrl || j.url || '');
      } catch (e) { /* 保持原文本 */ }
    }
    s = String(s).trim();
    if (!/^https?:\/\//i.test(s)) {
      throw new Error('返回值不是音频地址：' + (s.slice(0, 100) || '(空)'));
    }
    return s;
  }

  function fetchAudioUrl(id) {
    if (!id) return Promise.reject(new Error('拿不到 tingId'));
    return request(apiUrl('audio-url', id), false).then(normalizeUrl);
  }

  function fetchPlayInfo(id) {
    if (!id) return Promise.reject(new Error('拿不到 tingId'));
    return request(apiUrl('play-info', id), true);
  }

  /* ---------- 界面 ---------- */
  var panel = document.createElement('div');
  panel.id = 'ytb-audio-panel';
  panel.style.cssText = [
    'position:fixed', 'right:16px', 'bottom:16px', 'z-index:2147483000',
    'width:370px', 'max-width:calc(100vw - 32px)',
    'background:#fff', 'color:#222', 'border:1px solid #e2e5ea', 'border-radius:10px',
    'box-shadow:0 6px 26px rgba(0,0,0,.16)', 'overflow:hidden',
    'font:13px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif'
  ].join(';');
  panel.innerHTML = [
    '<div data-role="head" style="display:flex;align-items:center;justify-content:space-between;padding:9px 12px;background:#f7f8fa;border-bottom:1px solid #eceef2;cursor:pointer;user-select:none">',
    '  <strong style="font-size:13px">音频直链查看器</strong>',
    '  <span data-role="fold" style="color:#888">收起 ▾</span>',
    '</div>',
    '<div data-role="body" style="padding:10px 12px 12px">',
    '  <div style="color:#777;margin-bottom:6px">当前集：<code data-role="id" style="word-break:break-all"></code></div>',
    '  <textarea data-role="url" readonly spellcheck="false" placeholder="点「取 audio-url」，或直接在页面点播放自动捕获" style="width:100%;height:78px;box-sizing:border-box;resize:vertical;border:1px solid #dcdfe6;border-radius:6px;padding:6px 8px;font:12px/1.5 monospace;color:#1a1a1a;background:#fafbfc"></textarea>',
    '  <div style="display:flex;flex-wrap:wrap;gap:6px;margin-top:8px">',
    '    <button data-act="audio" type="button">取 audio-url</button>',
    '    <button data-act="info" type="button">取 play-info</button>',
    '    <button data-act="prev" type="button">上一集</button>',
    '    <button data-act="next" type="button">下一集</button>',
    '  </div>',
    '  <div style="display:flex;flex-wrap:wrap;gap:6px;margin-top:6px">',
    '    <button data-act="copy" type="button">复制地址</button>',
    '    <button data-act="open" type="button">新窗口打开</button>',
    '  </div>',
    '  <div data-role="status" style="margin-top:8px;color:#666;word-break:break-all"></div>',
    '</div>'
  ].join('');
  document.body.appendChild(panel);

  var q = function (sel) { return panel.querySelector(sel); };
  var out = q('[data-role="url"]');
  var idEl = q('[data-role="id"]');
  var statusEl = q('[data-role="status"]');
  var bodyEl = q('[data-role="body"]');
  Array.prototype.forEach.call(panel.querySelectorAll('button'), function (b) {
    b.style.cssText = 'padding:4px 9px;border:1px solid #d5d9e0;border-radius:5px;background:#fff;color:#333;cursor:pointer;font-size:12px';
  });

  function setStatus(msg, kind) {
    statusEl.textContent = msg;
    statusEl.style.color = kind === 'err' ? '#d03050' : (kind === 'ok' ? '#1a7f45' : '#666');
  }

  function setUrl(url, note) {
    out.value = url;
    console.log('[音频直链]', url);
    setStatus(note || '取到地址 ' + new Date().toLocaleTimeString(), 'ok');
  }

  function copyUrl() {
    var v = out.value.trim();
    if (!v) { setStatus('还没有地址可复制', 'err'); return; }
    if (typeof GM_setClipboard === 'function') {
      GM_setClipboard(v, 'text');
      setStatus('已复制到剪贴板', 'ok');
    } else {
      out.select();
      document.execCommand('copy');
      setStatus('已复制到剪贴板', 'ok');
    }
  }

  function guard(p) {
    setStatus('请求中…');
    return Promise.resolve(p).catch(function (e) {
      setStatus('失败：' + (e && e.message ? e.message : String(e)), 'err');
    });
  }

  function load(kind, id, note) {
    if (!id) { setStatus('拿不到该集的 tingId', 'err'); return; }
    if (kind === 'info') {
      guard(fetchPlayInfo(id).then(function (info) {
        if (!info || !info.audioUrl) throw new Error('play-info 里没有 audioUrl');
        setUrl(info.audioUrl, '来自 play-info：' + (info.tingTitle || '') +
          '（前集 ' + (info.prevTingId || '-') + ' / 后集 ' + (info.nextTingId || '-') + '）' +
          (note ? ' · ' + note : ''));
      }));
    } else {
      guard(fetchAudioUrl(id).then(function (u) {
        setUrl(u, '来自 audio-url' + (note ? ' · ' + note : ''));
      }));
    }
  }

  idEl.textContent = currentId() || '(无)';
  if (!deviceId()) {
    setStatus('未发现设备指纹 ' + DEVICE_KEY + '，先在页面点一次播放再试', 'err');
  } else {
    setStatus('就绪。点播放可自动捕获，或手动取地址');
  }

  q('[data-role="head"]').addEventListener('click', function () {
    var hidden = bodyEl.style.display === 'none';
    bodyEl.style.display = hidden ? '' : 'none';
    q('[data-role="fold"]').textContent = hidden ? '收起 ▾' : '展开 ▸';
  });

  panel.addEventListener('click', function (e) {
    var act = e.target && e.target.getAttribute && e.target.getAttribute('data-act');
    if (!act) return;
    if (act === 'audio') load('audio', currentId());
    else if (act === 'info') load('info', currentId());
    else if (act === 'prev') load('info', tingIdFrom(document.getElementById('prev-url')), '上一集');
    else if (act === 'next') load('info', tingIdFrom(document.getElementById('next-url')), '下一集');
    else if (act === 'copy') copyUrl();
    else if (act === 'open') {
      var v = out.value.trim();
      if (v) window.open(v, '_blank', 'noopener');
      else setStatus('还没有地址可打开', 'err');
    }
  });

  /* ---------- 被动捕获：不额外请求接口，也就不会触发限流 ---------- */
  if (audio) {
    ['loadstart', 'loadedmetadata', 'canplay', 'play', 'playing'].forEach(function (ev) {
      audio.addEventListener(ev, function () {
        var u = audio.currentSrc || audio.getAttribute('src') || '';
        if (/^https?:\/\//i.test(u) && u !== out.value) {
          setUrl(u, '页面播放自动捕获（' + ev + '）');
        }
      });
    });
  }

  // 站内切集不会重新加载页面，data-ting-id 会被改写，跟着更新显示
  new MutationObserver(function () {
    idEl.textContent = currentId() || '(无)';
  }).observe(card, { attributes: true, attributeFilter: ['data-ting-id'] });

  /* ---------- 油猴菜单 ---------- */
  if (typeof GM_registerMenuCommand === 'function') {
    GM_registerMenuCommand('取当前集直链并复制', function () {
      guard(fetchAudioUrl(currentId()).then(function (u) { setUrl(u); copyUrl(); }));
    });
    GM_registerMenuCommand('显示 / 隐藏直链面板', function () {
      q('[data-role="head"]').click();
    });
  }
})();
