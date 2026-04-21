'use strict';

// ---------------------------------------------------------------------------
// Keyboard navigation
// ---------------------------------------------------------------------------
document.addEventListener('keydown', function(e) {
  const tag = e.target.tagName.toLowerCase();
  const isInput = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;

  // 'Escape' → blur input first; once nothing is focused, go back to active search on detail page
  if (e.key === 'Escape') {
    if (isInput) { e.target.blur(); return; }
    const openDialogs = document.querySelectorAll('dialog[open]');
    if (openDialogs.length > 0) { openDialogs.forEach(function(d) { d.close(); }); return; }
    var detailPage = document.getElementById('detail-page');
    if (detailPage) {
      e.preventDefault();
      // If the page was reached via a Similar-images click (the server-set
      // data-ref marks this) walk the browser history one step. Using
      // history.back() rather than navigating to data-ref preserves the
      // full chain: the previous detail page is still the one the user saw,
      // with its own predecessors intact.
      if (detailPage.dataset.ref && history.length > 1) {
        history.back();
        return;
      }
      var backLink = document.querySelector('.back-link');
      if (backLink) { backLink.click(); }
      else { window.location.href = '/'; }
      return;
    }
  }

  // 'f' → favorite toggle on detail page
  if (e.key === 'f' && !isInput) {
    const favBtn = document.querySelector('.btn-fav');
    if (favBtn) favBtn.click();
    return;
  }

  // 's' → focus the search input (on any page that has one)
  if (e.key === 's' && !isInput) {
    const si = document.getElementById('search-input');
    if (si) { e.preventDefault(); si.focus(); si.select(); }
    return;
  }

  // 't' → focus the tag input (detail page)
  if (e.key === 't' && !isInput) {
    const tagInput = document.getElementById('tag-input');
    if (tagInput) { e.preventDefault(); tagInput.focus(); }
    return;
  }

  // 'Delete' → delete current image on detail page
  if ((e.key === 'Delete' || e.key === 'Del') && !isInput) {
    var delBtn = document.getElementById('delete-image-btn');
    if (delBtn) { e.preventDefault(); delBtn.click(); }
    return;
  }

  // Spacebar → play/pause the detail-page video. Guarded by !isInput so it
  // doesn't hijack spaces inside the tag or search inputs.
  if (e.key === ' ' && !isInput) {
    var vid = document.querySelector('.detail-video');
    if (vid) {
      e.preventDefault();
      if (vid.paused) vid.play(); else vid.pause();
    }
    return;
  }

  // 'h' → previous page, 'l' → next page in gallery
  if ((e.key === 'h' || e.key === 'l') && !isInput) {
    var pLinks = document.querySelectorAll('.pagination a');
    if (e.key === 'h') {
      var pPrev = Array.from(pLinks).find(function(a) { return a.textContent.indexOf('Prev') >= 0; });
      if (pPrev) { e.preventDefault(); pPrev.click(); }
    } else {
      var pNext = Array.from(pLinks).find(function(a) { return a.textContent.indexOf('Next') >= 0; });
      if (pNext) { e.preventDefault(); pNext.click(); }
    }
    return;
  }

  // Arrow keys: navigate gallery grid or prev/next on detail page
  if (!isInput && (e.key === 'ArrowLeft' || e.key === 'ArrowRight' || e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
    // Detail page: left/right = prev/next image
    if (document.getElementById('detail-page')) {
      if (e.key === 'ArrowLeft') {
        var prev = document.querySelector('.nav-arrow[title^="Previous image"]');
        if (prev) { e.preventDefault(); window.location.href = prev.href; }
      } else if (e.key === 'ArrowRight') {
        var next = document.querySelector('.nav-arrow[title^="Next image"]');
        if (next) { e.preventDefault(); window.location.href = next.href; }
      }
      return;
    }
    // Gallery: arrow keys navigate grid (Up/Down by row, Left/Right by cell)
    const cards = Array.from(document.querySelectorAll('.thumb-card'));
    if (cards.length === 0) return;
    const focused = document.querySelector('.thumb-card.focused');
    let idx = focused ? cards.indexOf(focused) : -1;
    if (idx < 0) idx = 0;
    else {
      // Calculate columns for Up/Down row navigation
      var cols = 1;
      if (cards.length > 1) {
        var grid = document.querySelector('.thumb-grid');
        if (grid && cards[0]) {
          var cardW = cards[0].offsetWidth;
          if (cardW > 0) cols = Math.max(1, Math.round(grid.offsetWidth / cardW));
        }
      }
      if (e.key === 'ArrowRight') idx = Math.min(idx + 1, cards.length - 1);
      else if (e.key === 'ArrowLeft') idx = Math.max(idx - 1, 0);
      else if (e.key === 'ArrowDown') idx = Math.min(idx + cols, cards.length - 1);
      else if (e.key === 'ArrowUp') idx = Math.max(idx - cols, 0);
    }
    cards.forEach(function(c) { c.classList.remove('focused'); });
    cards[idx].classList.add('focused');
    cards[idx].scrollIntoView({ block: 'nearest' });
    e.preventDefault();
    return;
  }

  // Enter: open focused card
  if (e.key === 'Enter' && !isInput) {
    const focused = document.querySelector('.thumb-card.focused a');
    if (focused) { window.location.href = focused.href; }
    return;
  }
});

// On a similar-click detail page (marked by data-ref), the "← Previous image"
// link walks browser history instead of navigating to its href so chains of
// any depth unwind one page at a time - matching the Escape keybinding. The
// href stays wired for cold loads (direct URL, bookmarked tab) where history
// has no predecessor.
document.addEventListener('click', function(e) {
  var link = e.target.closest('.back-link');
  if (!link) return;
  var detailPage = document.getElementById('detail-page');
  if (detailPage && detailPage.dataset.ref && history.length > 1) {
    e.preventDefault();
    history.back();
  }
});

// Delete-from-ref walks history instead of server-redirecting so the chain
// (A -> B -> C -> delete) lands back on B's original URL (with its own
// ref=A intact), leaving Escape free to unwind the rest of the chain.
// HX-Redirect would push a fresh history entry and silently drop the ref
// chain. The event's detail.fallback carries the redirect URL the handler
// would otherwise have set; we fall back to it when history has no
// predecessor (direct link, fresh tab).
document.body.addEventListener('delete-go-back', function(e) {
  if (history.length > 1) { history.back(); return; }
  var fallback = e.detail && e.detail.fallback;
  if (fallback) window.location.href = fallback;
});

// ---------------------------------------------------------------------------
// Gallery focus restore: when returning from a detail page via a back-link
// carrying #img-N, focus the matching thumbnail so the arrow-key cursor
// picks up where the user left off.
// ---------------------------------------------------------------------------
function restoreGalleryFocusFromHash() {
  var m = window.location.hash.match(/^#img-(\d+)$/);
  if (!m) return;
  var card = document.querySelector('.thumb-card[data-id="' + m[1] + '"]');
  if (!card) return;
  document.querySelectorAll('.thumb-card.focused').forEach(function(c) {
    c.classList.remove('focused');
  });
  card.classList.add('focused');
  card.scrollIntoView({ block: 'nearest' });
}
document.addEventListener('DOMContentLoaded', restoreGalleryFocusFromHash);

// ---------------------------------------------------------------------------
// Video hover preview swap (with error fallback to avoid "?" on hover fail)
// ---------------------------------------------------------------------------
document.addEventListener('mouseover', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  const hoverSrc = card.dataset.hover;
  if (!img || !hoverSrc || img.dataset.hovering) return;
  img.dataset.orig = img.src;
  img.dataset.hovering = '1';
  img.onerror = function() {
    img.src = img.dataset.orig || '';
    delete img.dataset.orig;
    delete img.dataset.hovering;
    img.onerror = null;
  };
  img.src = hoverSrc;
});

document.addEventListener('mouseout', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  if (!img || !img.dataset.orig) return;
  img.src = img.dataset.orig;
  delete img.dataset.orig;
  delete img.dataset.hovering;
  img.onerror = null;
});

// ---------------------------------------------------------------------------
// Sidebar tag filter (client-side substring match, no HTMX)
// ---------------------------------------------------------------------------
document.addEventListener('input', function(e) {
  if (e.target.id !== 'sidebar-filter') return;
  const q = e.target.value.toLowerCase();
  document.querySelectorAll('.tag-entry').forEach(function(el) {
    const name = (el.dataset.name || '').toLowerCase();
    el.style.display = (!q || name.includes(q)) ? '' : 'none';
  });
});

// Tag-input (detail page) clears the invalid-tag flash as soon as the user
// starts fixing the input, so the error isn't stuck on-screen through the
// next submit.
document.addEventListener('input', function(e) {
  if (e.target.id !== 'tag-input') return;
  var tagsDiv = document.getElementById('image-tags');
  if (!tagsDiv) return;
  var err = tagsDiv.querySelector('.flash-err');
  if (err) err.remove();
});

// ---------------------------------------------------------------------------
// Sidebar tag-add-btn: append tag to current search query
// ---------------------------------------------------------------------------
document.addEventListener('click', function(e) {
  const btn = e.target.closest('.tag-add-btn');
  if (!btn) return;
  e.preventDefault();
  const tagName = btn.dataset.tag;
  if (!tagName) return;
  const si = document.getElementById('search-input');
  if (!si) return;
  const terms = si.value.trim().split(/\s+/).filter(Boolean);
  if (!terms.includes(tagName)) terms.push(tagName);
  si.value = terms.join(' ');
  const form = document.getElementById('search-form');
  if (form && window.htmx) window.htmx.trigger(form, 'submit');
  else if (form) form.submit();
});

// ---------------------------------------------------------------------------
// Category collapse in sidebar
// ---------------------------------------------------------------------------
document.addEventListener('click', function(e) {
  const btn = e.target.closest('.cat-collapse');
  if (!btn) return;
  const group = btn.closest('.tag-group');
  if (!group) return;
  const list = group.querySelector('.tag-list-sidebar');
  if (!list) return;
  const collapsed = list.style.display === 'none';
  list.style.display = collapsed ? '' : 'none';
  btn.textContent = collapsed ? '▾' : '▸';
});

// ---------------------------------------------------------------------------
// Batch selection: show/hide batch bar and keep checkboxes visible
// ---------------------------------------------------------------------------
document.addEventListener('change', function(e) {
  if (!e.target.classList.contains('thumb-checkbox')) return;
  updateBatchBar();
});

function updateBatchBar() {
  const checked = document.querySelectorAll('.thumb-checkbox:checked');
  const bar = document.getElementById('batch-bar');
  const grid = document.getElementById('gallery-grid');
  if (bar) bar.classList.toggle('visible', checked.length > 0);
  if (grid) grid.classList.toggle('batch-active', checked.length > 0);
  const countEl = document.getElementById('batch-count');
  if (countEl) countEl.textContent = checked.length + ' selected';
  // Hide the header's Delete-all so users aiming for Delete-selected can't misclick.
  const delAll = document.getElementById('delete-search-btn');
  if (delAll) delAll.hidden = checked.length > 0;
}

function clearSelection() {
  document.querySelectorAll('.thumb-checkbox:checked').forEach(function(cb) { cb.checked = false; });
  updateBatchBar();
}

function selectAll() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) { cb.checked = true; });
  updateBatchBar();
}

// Batch delete: populate the confirmation dialog with the selected count and
// show it. The actual fetch + background-job wiring lives in confirmDeleteSelected
// (gallery.html) alongside the sibling delete-all-search flow.
function batchDeleteSelected() {
  var checked = document.querySelectorAll('.thumb-checkbox:checked');
  if (checked.length === 0) return;
  var countEl = document.getElementById('delete-selected-count');
  if (countEl) countEl.textContent = checked.length;
  var nounEl = document.getElementById('delete-selected-noun');
  if (nounEl) nounEl.textContent = checked.length === 1 ? 'image' : 'images';
  var flash = document.getElementById('delete-selected-flash');
  if (flash) flash.innerHTML = '';
  var dlg = document.getElementById('delete-selected-dialog');
  if (dlg) dlg.showModal();
}

// refreshJobStatus forces the top-right job-status widget to re-fetch its
// state so a newly started background job becomes visible without waiting for
// the next 2s poll tick.
function refreshJobStatus() {
  var el = document.getElementById('job-status');
  if (!el || !window.htmx) return;
  el.setAttribute('hx-trigger', 'every 2s');
  window.htmx.process(el);
  window.htmx.ajax('GET', '/internal/job/status', {target: '#job-status', swap: 'outerHTML'});
}

// ---------------------------------------------------------------------------
// Shared confirmation dialog. Replaces native confirm() and hx-confirm with
// the same <dialog> modal style used by rename/save-search/etc.
//   showConfirm(message, onOk, danger?)  - explicit callers (fetch handlers)
//   htmx:confirm event listener          - intercepts hx-confirm attributes
// An hx-confirm element may set data-confirm-danger="..." for a second,
// red-tinted line of warning text (same pattern as the bulk-delete dialog).
// ---------------------------------------------------------------------------
function showConfirm(message, onOk, danger, okLabel) {
  var dlg = document.getElementById('confirm-dialog');
  if (!dlg) { if (window.confirm(message)) onOk(); return; }
  document.getElementById('confirm-dialog-msg').textContent = message || '';
  document.getElementById('confirm-dialog-danger').textContent = danger || '';
  var okBtn = document.getElementById('confirm-dialog-ok');
  var cancelBtn = document.getElementById('confirm-dialog-cancel');
  okBtn.textContent = okLabel || 'OK';
  var close = function() { dlg.close(); okBtn.onclick = null; cancelBtn.onclick = null; };
  okBtn.onclick = function() { close(); onOk(); };
  cancelBtn.onclick = close;
  dlg.showModal();
}

document.body.addEventListener('htmx:confirm', function(e) {
  if (!e.detail || !e.detail.question) return;
  e.preventDefault();
  var ds = e.detail.elt && e.detail.elt.dataset ? e.detail.elt.dataset : {};
  showConfirm(e.detail.question, function() { e.detail.issueRequest(true); }, ds.confirmDanger, ds.confirmOk);
});

// ---------------------------------------------------------------------------
// Page jump: clicking the "Page X / Y" button opens a dialog to navigate to a
// specific page. Handles both the gallery (HTMX-driven pagination) and the
// tags page (full-page navigation) by setting ?page= on the current URL.
// ---------------------------------------------------------------------------
document.addEventListener('click', function(e) {
  var btn = e.target.closest('.page-jump');
  if (!btn) return;
  e.preventDefault();
  var dlg = document.getElementById('page-jump-dialog');
  var input = document.getElementById('page-jump-input');
  var totalSpan = document.getElementById('page-jump-total');
  if (!dlg || !input) return;
  var current = btn.dataset.current || '1';
  var total = btn.dataset.total || '1';
  input.value = current;
  // data-max, not the HTML max attribute: max would trigger HTML5
  // constraint validation and block submit, so the clamp never runs.
  input.dataset.max = total;
  if (totalSpan) totalSpan.textContent = total;
  dlg.showModal();
  setTimeout(function() { input.focus(); input.select(); }, 0);
});

function submitPageJump() {
  var input = document.getElementById('page-jump-input');
  if (!input) return;
  var p = parseInt(input.value, 10);
  if (!p || p < 1) p = 1;
  var max = parseInt(input.dataset.max, 10);
  if (max && p > max) p = max;
  var u = new URL(window.location.href);
  u.searchParams.set('page', String(p));
  window.location.href = u.toString();
}

// ---------------------------------------------------------------------------
// Sidebar toggle (narrow viewports)
// ---------------------------------------------------------------------------
document.addEventListener('click', function(e) {
  if (!e.target.id || e.target.id !== 'sidebar-toggle') return;
  const sidebar = document.getElementById('sidebar');
  if (sidebar) sidebar.classList.toggle('open');
});

// ---------------------------------------------------------------------------
// Folder tree: expand/collapse with cookie persistence
// ---------------------------------------------------------------------------
function getFolderCookie() {
  var m = document.cookie.match(/monbooru_folders=([^;]*)/);
  if (!m) return new Set();
  try { return new Set(decodeURIComponent(m[1]).split(',').filter(Boolean)); }
  catch (err) { return new Set(); }
}

function setFolderCookie(set) {
  document.cookie = 'monbooru_folders=' + encodeURIComponent(Array.from(set).join(',')) + '; path=/; max-age=31536000';
}

function toggleFolderItem(btn, targetId, path) {
  var list = document.getElementById(targetId);
  if (!list) return;
  var state = getFolderCookie();
  var isCollapsed = list.style.display === 'none';
  list.style.display = isCollapsed ? '' : 'none';
  btn.textContent = isCollapsed ? '▼' : '▶';
  if (isCollapsed) state.add(path || targetId);
  else state.delete(path || targetId);
  setFolderCookie(state);
}

function initSectionToggle(toggleId, listId, cookieKey, forceOpen) {
  var toggle = document.getElementById(toggleId);
  var list = document.getElementById(listId);
  if (!toggle || !list) return;
  var expanded = getFolderCookie();
  if (forceOpen || expanded.has(cookieKey)) {
    list.style.display = '';
    toggle.textContent = '▼';
  }
  toggle.onclick = function() {
    var state = getFolderCookie();
    var isCollapsed = list.style.display === 'none';
    list.style.display = isCollapsed ? '' : 'none';
    toggle.textContent = isCollapsed ? '▼' : '▶';
    if (isCollapsed) state.add(cookieKey);
    else state.delete(cookieKey);
    setFolderCookie(state);
  };
}

function initFolderTree() {
  var expanded = getFolderCookie();

  // Determine current folder from URL query. Both folder:PATH (recursive)
  // and folderonly:PATH (exact) drive the same sidebar auto-expand path.
  var currentFolder = '';
  var urlParams = new URLSearchParams(window.location.search);
  var q = urlParams.get('q') || '';
  var folderMatch = q.match(/(?:^|\s)folder(?:only)?:(?:"([^"]+)"|([^\s]*))/);
  if (folderMatch) {
    currentFolder = folderMatch[1] || folderMatch[2];
  }

  // Force the Source section open when the current query targets a source,
  // and its AI subtree when the query picks an AI variant.
  var sourceMatch = q.match(/(?:^|\s)source:([a-z0-9_,-]+)/i);
  var sourceVal = sourceMatch ? sourceMatch[1].toLowerCase() : '';
  var sourceOpen = sourceVal !== '';
  var sourceAIOpen = sourceVal === 'ai' || sourceVal === 'a1111' || sourceVal === 'comfyui';

  // Folder tree main toggle (show/hide whole tree).
  // Use onclick assignment (not addEventListener) to prevent duplicate handlers
  // from multiple calls (e.g. HTMX partial swaps fire htmx:afterSettle repeatedly).
  initSectionToggle('folder-tree-toggle', 'folder-tree-list', '__tree__', false);
  initSectionToggle('source-tree-toggle', 'source-tree-list', '__source__', sourceOpen);
  var treeToggle = document.getElementById('folder-tree-toggle');
  var treeList = document.getElementById('folder-tree-list');

  // Subfolder toggles - use onclick to avoid duplicate listeners
  document.querySelectorAll('.folder-toggle-btn[data-path]').forEach(function(btn) {
    var path = btn.dataset.path;
    var targetId = btn.dataset.target || ('fc-' + path);
    var list = document.getElementById(targetId);
    if (!list) return;

    // urlDriven: this expansion comes from navigation context (current folder
    // or active source filter). Gated by firstInit so a user collapse isn't
    // undone on the next htmx settle - once the button has been seen, its
    // open/close state is the cookie's to decide.
    var firstInit = !btn.dataset.folderInit;
    btn.dataset.folderInit = '1';
    var urlDriven = (currentFolder && (currentFolder === path || currentFolder.startsWith(path + '/'))) ||
      (path === '__source_ai__' && sourceAIOpen);
    var shouldExpand = expanded.has(path) || (firstInit && urlDriven);

    if (shouldExpand) {
      list.style.display = '';
      btn.textContent = '▼';
      // Force the parent tree open only when navigation drives this
      // expansion. The parent's open/close otherwise stays under the
      // user's control via initSectionToggle's own cookie key.
      if (firstInit && urlDriven && treeList && path !== '__source_ai__') {
        treeList.style.display = '';
        if (treeToggle) treeToggle.textContent = '▼';
      }
    }

    btn.onclick = function(e) {
      e.stopPropagation();
      toggleFolderItem(btn, targetId, path);
    };
  });
}

document.addEventListener('DOMContentLoaded', initFolderTree);
// Re-run on HTMX settle to restore auto-expand state after URL changes,
// but since we use btn.onclick (not addEventListener) no duplicate handlers accumulate.
document.addEventListener('htmx:afterSettle', initFolderTree);

// ---------------------------------------------------------------------------
// Tag suggest (detail page + merge dialog): apply selected suggestion
// ---------------------------------------------------------------------------
function applyTagSuggest(btn) {
  var tagName = btn.dataset.tagName;
  if (!tagName) return;
  var dd = btn.closest('.suggest-dropdown');
  if (!dd) return;
  dd.innerHTML = '';
  var container = dd.parentElement;
  if (!container) return;
  var input = container.querySelector('input[type="text"]');
  if (!input) return;
  // Multi-tag inputs (e.g. upload form) keep previous tokens: replace the last
  // whitespace-separated word and append a trailing space for the next tag.
  if (input.dataset.multiTags) {
    var words = input.value.split(/(\s+)/);
    var lastIdx = -1;
    for (var i = words.length - 1; i >= 0; i--) {
      if (words[i].trim() !== '') { lastIdx = i; break; }
    }
    if (lastIdx >= 0) words[lastIdx] = tagName;
    else words.push(tagName);
    input.value = words.join('') + ' ';
    input.focus();
    return;
  }
  input.value = tagName;
  input.focus();
  // Submit the tag-add form if present; merge dialog just fills the input
  var form = input.closest('form') || container.querySelector('form');
  if (form && form.id === 'add-tag-form') {
    form.requestSubmit();
  }
}

// ---------------------------------------------------------------------------
// Folder suggest: apply selected folder path to the input that opened the dropdown.
// Used by the move-image and move-selected dialogs. Keeps focus on the input so
// the user can keep typing; the dialog's submit button finishes the move.
// ---------------------------------------------------------------------------
function applyFolderSuggest(btn) {
  var folder = btn.dataset.folderPath;
  if (folder == null) return;
  var dd = btn.closest('.suggest-dropdown');
  if (!dd) return;
  var container = dd.parentElement;
  if (!container) return;
  var input = container.querySelector('input[type="text"]');
  if (!input) return;
  dd.innerHTML = '';
  input.value = folder;
  input.focus();
}

// ---------------------------------------------------------------------------
// Search suggest: apply selected suggestion to search input
// ---------------------------------------------------------------------------
function applySearchSuggest(tagName) {
  var si = document.getElementById('search-input');
  if (!si) return;
  var words = si.value.split(/(\s+)/);
  // Find last non-whitespace word
  var lastWordIdx = -1;
  for (var i = words.length - 1; i >= 0; i--) {
    if (words[i].trim() !== '') { lastWordIdx = i; break; }
  }
  if (lastWordIdx >= 0) {
    var last = words[lastWordIdx];
    var prefix = last.startsWith('-') ? '-' : '';
    words[lastWordIdx] = prefix + tagName;
  } else {
    words.push(tagName);
  }
  si.value = words.join('') + ' ';
  var dd = document.getElementById('search-suggest');
  if (dd) dd.innerHTML = '';
  si.focus();
}

// ---------------------------------------------------------------------------
// Auto-reload gallery/tags after job completes; auto-clear status after 30s
// ---------------------------------------------------------------------------
var _jobAutoClearTimer = null;
// FinishedAt the current auto-clear timer was armed against; re-armed on newer
// surface events so rolling watcher activity doesn't trip the dismiss mid-batch
// (which would strip hx-trigger and silence the widget until page reload).
var _jobAutoClearFinishedAt = '';
// Track the FinishedAt timestamp of the last reloaded event so each new
// watcher/job completion triggers exactly one reload.
var _lastReloadedFinishedAt = '';
// Processed cursor for the currently-running job. The gallery grid is
// refreshed whenever the worker bumps this so changes show up without a
// manual reload, mirroring the watcher's per-event surface path. Applies
// to progress-emitting job types listed in the handler below.
var _lastJobProcessed = -1;
// Watcher-event counter. Bumped by the server whenever the filesystem
// watcher surfaces an ingest/remove while a job is running; gives the grid
// a refresh signal for job types that don't themselves change the image
// list (autotag) or whose progress cursor is scoped to existing rows.
var _lastWatcherNotices = -1;
// One-shot latch set by user-initiated deletes. When the delete job finishes,
// the htmx.ajax reload path intermittently fails to settle the gallery-grid
// swap, leaving the view stale. Force a full reload instead - only for the
// delete case; other job completions still use the incremental swap.
var _pendingGalleryReload = false;

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail.elt;

  // When the gallery grid is swapped (pagination, search, or job reload),
  // reset batch selection state to match the fresh (all-unchecked) checkboxes.
  // The swap wipes any .focused class; reapply it from the URL hash so the
  // arrow-key cursor doesn't vanish when a post-job refresh races the user.
  if (el && el.id === 'gallery-grid') {
    clearSelection();
    restoreGalleryFocusFromHash();
    return;
  }

  if (!el || el.id !== 'job-status') return;

  var isDone = !!el.querySelector('.job-done');
  var isErr  = !!el.querySelector('.job-error');
  // `job-running` lives on #job-status itself, not a descendant, so use
  // classList instead of querySelector (which would always miss it).
  var isRunning = el.classList.contains('job-running');
  var finishedAt = el.dataset.finishedAt || '';

  // Running progress-emitting jobs: refresh the gallery grid whenever the
  // worker's Processed cursor advances so new ingests, deletions, and
  // re-extractions show up without a manual reload. The listed types call
  // jobs.Update(processed,…) inside their worker loops; others either
  // finish quickly (watcher events surface via FinishedAt) or don't
  // visibly alter the gallery during the run. Sits above the isIdle bail
  // so the running state reaches this branch.
  //
  // WatcherNotices covers the other half: the filesystem watcher may ingest
  // or remove files while any job runs, and autotag's own cursor doesn't
  // reflect those. A bump triggers the same grid refresh regardless of job
  // type (sync drops watcher events upstream, so its counter stays at 0).
  var jobType = el.dataset.jobType || '';
  var processed = parseInt(el.dataset.processed || '0', 10);
  var watcherNotices = parseInt(el.dataset.watcherNotices || '0', 10);
  var refreshDuringRun = jobType === 'sync' || jobType === 'delete' || jobType === 're-extract';
  if (!isRunning) {
    _lastJobProcessed = -1;
    _lastWatcherNotices = -1;
  } else {
    var needRefresh = false;
    if (refreshDuringRun && processed > 0 && processed !== _lastJobProcessed) {
      _lastJobProcessed = processed;
      needRefresh = true;
    }
    if (watcherNotices > 0 && watcherNotices !== _lastWatcherNotices) {
      _lastWatcherNotices = watcherNotices;
      needRefresh = true;
    }
    if (needRefresh) {
      var runningGrid = document.getElementById('gallery-grid');
      if (runningGrid && window.htmx) {
        var runURL = new URL(window.location.href);
        window.htmx.ajax('GET', runURL.pathname + runURL.search, {target: '#gallery-grid', swap: 'innerHTML'});
      }
    }
  }

  var isIdle = !isDone && !isErr && !isRunning;

  // Reset auto-clear flag when job-status goes idle (dismissed)
  if (isIdle) {
    _jobAutoClearFinishedAt = '';
    if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
    return;
  }

  // Auto-clear 30s after the last surface event. Re-arm whenever FinishedAt
  // advances so rolling watcher events during a batch keep the widget alive.
  if ((isDone || isErr) && finishedAt && finishedAt !== _jobAutoClearFinishedAt) {
    _jobAutoClearFinishedAt = finishedAt;
    if (_jobAutoClearTimer) clearTimeout(_jobAutoClearTimer);
    _jobAutoClearTimer = setTimeout(function() {
      _jobAutoClearFinishedAt = '';
      dismissJobStatus();
    }, 30000);
  }

  // Post-delete: full reload guarantees the gallery reflects the deletions.
  if (isDone && _pendingGalleryReload) {
    _pendingGalleryReload = false;
    if (finishedAt) _lastReloadedFinishedAt = finishedAt;
    if (document.getElementById('gallery-grid')) {
      window.location.reload();
      return;
    }
  }

  // Reload gallery grid or detail tags once per completion event. The
  // data-finished-at attribute changes whenever the server records a new
  // event (e.g. sync complete, watcher add/remove), so reloads no longer
  // latch on a single flag.
  if (isDone && finishedAt && finishedAt !== _lastReloadedFinishedAt) {
    _lastReloadedFinishedAt = finishedAt;

    // Gallery page: reload grid
    var grid = document.getElementById('gallery-grid');
    if (grid) {
      var url = new URL(window.location.href);
      if (window.htmx) {
        window.htmx.ajax('GET', url.pathname + url.search, {target: '#gallery-grid', swap: 'innerHTML'});
      }
    }

    // Detail page: reload tag list and clear autotag status message. The
    // clear is guarded by the detail-page min-visible window (see
    // _autotagFlashShownAt in detail.html) so a sub-2s auto-tag doesn't wipe
    // the "started" flash before the user can read it.
    var imageTags = document.getElementById('image-tags');
    if (imageTags) {
      var imageId = imageTags.dataset.imageId;
      if (imageId && window.htmx) {
        window.htmx.ajax('GET', '/images/' + imageId + '/tags', {target: '#image-tags', swap: 'outerHTML'});
      }
      var ar = document.getElementById('autotag-result');
      if (ar && ar.innerHTML.trim() !== '') {
        var shown = window._autotagFlashShownAt || 0;
        var elapsed = Date.now() - shown;
        var minMs = 3000;
        if (elapsed >= minMs) {
          ar.innerHTML = '';
        } else {
          setTimeout(function() {
            var ar2 = document.getElementById('autotag-result');
            if (ar2) ar2.innerHTML = '';
          }, minMs - elapsed);
        }
      }
    }
  }
});

function getCSRFToken() {
  var meta = document.querySelector('meta[name="csrf-token"]');
  if (meta) return meta.content;
  var input = document.querySelector('input[name="_csrf"]');
  return input ? input.value : '';
}

function dismissJobStatus() {
  _lastReloadedFinishedAt = '';
  _lastJobProcessed = -1;
  _lastWatcherNotices = -1;
  _jobAutoClearFinishedAt = '';
  if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
  // Call backend to clear job state, then clear the UI
  var csrf = getCSRFToken();
  fetch('/internal/job/dismiss', {
    method: 'POST',
    headers: {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf},
    body: '_csrf=' + encodeURIComponent(csrf)
  }).catch(function() {});
  var js = document.getElementById('job-status');
  if (js) {
    js.innerHTML = '';
    js.removeAttribute('hx-trigger');
    if (window.htmx) window.htmx.process(js);
  }
}

// cancelJobStatus interrupts the running auto-tagging job. The worker observes
// ctx.Done() and wraps up via Complete; the 30s auto-dismiss takes over from
// there so the user still sees a "cancelled" summary on the status bar.
function cancelJobStatus() {
  var csrf = getCSRFToken();
  fetch('/internal/job/cancel', {
    method: 'POST',
    headers: {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf},
    body: '_csrf=' + encodeURIComponent(csrf)
  }).catch(function() {});
}

// ---------------------------------------------------------------------------
// Shared suggest-dropdown keyboard navigation (used by search, tag input, merge)
// ---------------------------------------------------------------------------
function handleSuggestKey(e, dropdownId, inputId) {
  var dd = document.getElementById(dropdownId);
  if (!dd) return;
  var items = Array.from(dd.querySelectorAll('.suggest-item'));
  var focused = dd.querySelector('.suggest-item.kbd-focused');
  var idx = focused ? items.indexOf(focused) : -1;
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    items.forEach(function(i){ i.classList.remove('kbd-focused'); });
    idx = Math.min(idx + 1, items.length - 1);
    if (idx >= 0) items[idx].classList.add('kbd-focused');
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    items.forEach(function(i){ i.classList.remove('kbd-focused'); });
    idx = Math.max(idx - 1, 0);
    if (idx >= 0) items[idx].classList.add('kbd-focused');
  } else if (e.key === 'Enter' && focused) {
    e.preventDefault();
    focused.click();
  } else if (e.key === 'Escape') {
    dd.innerHTML = '';
  }
}

// Close suggest dropdown when clicking outside
function initSuggestDismiss(dropdownId, inputId) {
  document.addEventListener('click', function(e) {
    var dd = document.getElementById(dropdownId);
    if (dd && !dd.contains(e.target) && e.target.id !== inputId) {
      dd.innerHTML = '';
    }
  });
}

// Initialize all known suggest dropdowns
initSuggestDismiss('search-suggest', 'search-input');
initSuggestDismiss('tag-suggest-dropdown', 'tag-input');
initSuggestDismiss('merge-suggest', 'merge-canon-input');
initSuggestDismiss('move-selected-suggest', 'move-selected-folder');
initSuggestDismiss('move-image-suggest', 'move-image-folder');

// ---------------------------------------------------------------------------
// Detail page: separate tags added during the current session from the rest.
// The "just-added" list is populated from tags that appear after initial load
// and is cleared on full page reload.
// ---------------------------------------------------------------------------
var _initialTagIDs = null;   // Set of tag IDs present on first load
var _addedTagOrder = [];     // Tag IDs in the order the user added them during this session

function captureInitialTags(container) {
  if (_initialTagIDs !== null) return;
  _initialTagIDs = new Set();
  container.querySelectorAll('.tag-list > li.tag-item[data-tag-id]').forEach(function(li) {
    _initialTagIDs.add(li.dataset.tagId);
  });
}

// Always keep auto-tagged items in their tagger subcategory; never treat
// them as "just added" even when an auto-tag run happens mid-session.
function registerInitialAutoTags(container) {
  if (_initialTagIDs === null) return;
  container.querySelectorAll('.tag-list > li.tag-item[data-source="auto"][data-tag-id]').forEach(function(li) {
    _initialTagIDs.add(li.dataset.tagId);
  });
}

function separateNewTags(container) {
  if (!container) return;
  if (_initialTagIDs === null) { captureInitialTags(container); return; }
  // Auto-tag runs that happened after page load expose new auto tags in their
  // tagger subcategory; pin them into the initial set so they don't drift into
  // "Just added" (which is reserved for tags the user typed in this session).
  registerInitialAutoTags(container);

  var added = container.querySelector('.tag-list-added');
  var divider = container.querySelector('.tag-list-divider');
  var title = container.querySelector('.tag-list-added-title');
  if (!added) return;

  // Scan every user-source tag-item across all subcategory lists for this image.
  var nodesById = {};
  container.querySelectorAll('.tag-list:not(.tag-list-added) li.tag-item[data-source="user"][data-tag-id]').forEach(function(li) {
    var id = li.dataset.tagId;
    if (_initialTagIDs.has(id)) return;
    nodesById[id] = li;
    if (_addedTagOrder.indexOf(id) === -1) _addedTagOrder.push(id);
  });
  _addedTagOrder = _addedTagOrder.filter(function(id) { return nodesById[id] !== undefined; });

  // appendChild on an existing node moves it; insertion order is preserved.
  _addedTagOrder.forEach(function(id) { added.appendChild(nodesById[id]); });

  // Hide subcategory groups (e.g. "Tags added by the user") whose tag list
  // just became empty after we moved items to "Just added". Without this the
  // header stays visible above an empty list on first add.
  container.querySelectorAll('.tag-source-group').forEach(function(group) {
    var list = group.querySelector('.tag-list:not(.tag-list-added)');
    group.hidden = !!list && list.children.length === 0;
  });

  var hasAdded = added.children.length > 0;
  if (divider) divider.hidden = !hasAdded;
  if (title) title.hidden = !hasAdded;
}

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail ? e.detail.elt : null;
  if (el && el.id === 'image-tags') separateNewTags(el);
});

document.addEventListener('DOMContentLoaded', function() {
  var el = document.getElementById('image-tags');
  if (el) captureInitialTags(el);
});

// ---------------------------------------------------------------------------
// Save search: update hidden query field when dialog opens
// ---------------------------------------------------------------------------
document.addEventListener('click', function(e) {
  if (e.target.id !== 'save-search-btn') return;
  var si = document.getElementById('search-input');
  var sq = document.getElementById('save-search-query');
  var sp = document.getElementById('save-search-preview');
  if (si && sq) {
    sq.value = si.value;
  }
  if (si && sp) {
    sp.textContent = si.value || '(empty)';
  }
});
