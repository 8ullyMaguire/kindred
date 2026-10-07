// Kindred JavaScript - minimal enhancement
// Target: <5KB minified

(() => {
  // Utility
  const $ = (s, c = document) => c.querySelector(s);
  const $$ = (s, c = document) => c.querySelectorAll(s);
  const debounce = (f, d) => {
    let t;
    return (...a) => { clearTimeout(t); t = setTimeout(() => f.apply(this, a), d); };
  };

  // 5.1 Tag autocomplete
  $$('input[data-autocomplete="tags"]').forEach(inp => {
    let timeout, value = '', active = -1, sugg = [];
    const box = document.createElement('ul');
    box.role = 'listbox';
    box.className = 'autocomplete-box';
    inp.parentNode.insertBefore(box, inp.nextSibling);
    const show = items => {
      sugg = items; active = -1;
      box.innerHTML = '';
      if (!items.length) { box.style.display = 'none'; return; }
      items.forEach((it, i) => {
        const li = document.createElement('li');
        li.role = 'option';
        li.textContent = it.name;
        li.dataset.i = i;
        li.onmouseenter = () => {
          $$('[role="option"]', box).forEach(o => o.setAttribute('aria-selected', 'false'));
          li.setAttribute('aria-selected', 'true');
          active = i;
        };
        li.onclick = () => { select(i); };
        box.appendChild(li);
      });
      box.style.display = 'block';
    };
    const select = i => {
      if (sugg[i]) {
        inp.value = sugg[i].name;
        value = inp.value;
        box.style.display = 'none';
        if (inp.dataset.autocompleteSubmit === 'true') inp.form.requestSubmit();
      }
    };
    const handle = debounce(async e => {
      const v = e.target.value.trim();
      if (v.length < 2) { box.style.display = 'none'; return; }
      if (v === value) return;
      value = v;
      try {
        const res = await fetch(`/api/v1/ao3/tags?q=${encodeURIComponent(v)}&per_page=10`);
        const data = await res.json();
        show(data || []);
      } catch { box.style.display = 'none'; }
    }, 150);
    inp.addEventListener('input', handle);
    inp.addEventListener('keydown', e => {
      if (box.style.display !== 'block') return;
      switch (e.key) {
        case 'ArrowDown': e.preventDefault(); active = (active + 1) % sugg.length; break;
        case 'ArrowUp': e.preventDefault(); active = (active - 1 + sugg.length) % sugg.length; break;
        case 'Enter': e.preventDefault(); if (active >= 0) select(active); break;
        case 'Escape': e.preventDefault(); box.style.display = 'none';
      }
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        $$('[role="option"]', box).forEach((o, i) => o.setAttribute('aria-selected', i === active ? 'true' : 'false'));
        box.scrollTop = active * box.firstChild.offsetHeight;
      }
    });
    document.addEventListener('click', e => { if (!inp.contains(e.target) && !box.contains(e.target)) box.style.display = 'none'; });
  });

  // 5.2 Keyboard navigation
  const main = $('main[data-keynav]');
  if (main) {
    const cards = $$('.work-card');
    let focus = -1;
    const setFocus = i => {
      focus = i;
      cards.forEach((c, idx) => { c.classList.toggle('focused', idx === focus); if (idx === focus) c.scrollIntoView({block: 'nearest'}); });
    };
    document.addEventListener('keydown', e => {
      if (e.target.tagName.match(/INPUT|TEXTAREA|SELECT/)) return;
      switch (e.key) {
        case 'j': case 'ArrowDown': e.preventDefault(); setFocus((focus + 1) % cards.length); break;
        case 'k': case 'ArrowUp': e.preventDefault(); setFocus((focus - 1 + cards.length) % cards.length); break;
        case 'Enter': if (focus >= 0) { e.preventDefault(); const link = cards[focus].querySelector('.work-card__title a'); if (link) link.click(); } break;
        case 'o': case 'O': if (focus >= 0) { e.preventDefault(); const ao3 = cards[focus].querySelector('.work-card__action--ao3'); if (ao3) window.open(ao3.href, '_blank'); } break;
        case 'm': case 'M': if (focus >= 0) { e.preventDefault(); const more = cards[focus].querySelector('.work-card__action--more'); if (more) more.click(); } break;
        case 'r': case 'R': if (focus >= 0) { e.preventDefault(); const btn = cards[focus].querySelector('.work-card__action--read button[type="submit"]'); if (btn) btn.form.requestSubmit(); } break;
        case '?': e.preventDefault(); showShortcuts(); break;
        case '/': e.preventDefault(); const search = $('.header input[type="text"]'); if (search) search.focus(); break;
      }
    });
    if (cards.length && focus === -1) setFocus(0);
  }

  // 5.3 Inline Read it
  $$('form.work-card__action--read').forEach(form => {
    form.addEventListener('submit', async e => {
      e.preventDefault();
      const btn = form.querySelector('button[type="submit"]');
      const txt = btn.textContent;
      btn.textContent = 'Saving...';
      btn.disabled = true;
      try {
        const res = await fetch(form.action, {method: 'POST', body: new FormData(form), credentials: 'same-origin'});
        if (!res.ok) throw new Error('Failed');
        btn.textContent = '✓ Marked as read';
        btn.disabled = true;
      } catch {
        btn.textContent = txt;
        btn.disabled = false;
        form.requestSubmit();
      }
    });
  });

  // 5.4 Rapid-fire arena
  if (location.pathname.startsWith('/arena')) {
    const form = $('form[action="/arena/compare"]');
    if (form) {
      let batch = [], timer = null;
      const toggle = document.createElement('label');
      toggle.className = 'rapid-toggle';
      toggle.innerHTML = '<input type="checkbox"> Rapid fire';
      form.parentNode.insertBefore(toggle, form.nextSibling);
      const flush = async () => {
        if (!batch.length) return;
        try {
          await fetch('/api/v1/arena/batch', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(batch), credentials: 'same-origin'});
          batch = [];
        } catch { /* fallback omitted for brevity */ }
      };
      toggle.querySelector('input').addEventListener('change', e => {
        if (e.target.checked) {
          form.querySelector('button[type="submit"]').disabled = true;
          form.addEventListener('submit', handle);
        } else {
          form.querySelector('button[type="submit"]').disabled = false;
          form.removeEventListener('submit', handle);
          flush();
        }
      });
      const handle = e => {
        e.preventDefault();
        const f = e.target;
        batch.push({winner: f.querySelector('input[name="winner"]').value, loser: f.querySelector('input[name="loser"]').value});
        fetch('/api/v1/arena/pair').then(r => r.json()).then(p => {
          if (p && p.work1 && p.work2) {
            f.querySelector('input[name="winner"]').value = '';
            f.querySelector('input[name="loser"]').value = '';
            f.querySelector('input[id="work1"]').value = p.work1;
            f.querySelector('input[id="work2"]').value = p.work2;
          }
        }).catch(() => {});
        if (batch.length >= 5) flush();
        else { clearTimeout(timer); timer = setTimeout(flush, 30000); }
      };
      window.addEventListener('beforeunload', () => { if (batch.length) navigator.sendBeacon ? navigator.sendBeacon('/api/v1/arena/batch', JSON.stringify(batch)) : flush(); });
    }
  }

  // Shortcuts modal
  function showShortcuts() {
    const dlg = document.createElement('dialog');
    dlg.innerHTML = `
      <h2>Keyboard Shortcuts</h2>
      <ul>
        <li><kbd>j</kbd> / <kbd>↓</kbd>: Next card</li>
        <li><kbd>k</kbd> / <kbd>↑</kbd>: Previous card</li>
        <li><kbd>Enter</kbd>: Open card</li>
        <li><kbd>o</kbd>: Open AO3</li>
        <li><kbd>m</kbd>: More like this</li>
        <li><kbd>r</kbd>: Mark as read</li>
        <li><kbd>?</kbd>: Show help</li>
        <li><kbd>/</kbd>: Focus search</li>
      </ul>
      <button autofocus>Close</button>
    `;
    dlg.querySelector('button').onclick = () => dlg.close();
    dlg.addEventListener('cancel', () => dlg.remove());
    document.body.appendChild(dlg);
    dlg.showModal();
  }
})();