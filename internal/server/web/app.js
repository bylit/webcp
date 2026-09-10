const state = {
  downloads: [],
  defaultPath: '',
  selectedPath: '',
  browserPath: '',
  browserParent: '',
  filter: 'all',
  pendingDelete: null,
  busy: new Set(),
};

const elements = {
  form: document.querySelector('#download-form'),
  url: document.querySelector('#url-input'),
  folderButton: document.querySelector('#folder-button'),
  folderPath: document.querySelector('#folder-path'),
  submit: document.querySelector('#submit-button'),
  error: document.querySelector('#form-error'),
  list: document.querySelector('#download-list'),
  filters: document.querySelector('#filters'),
  resultCount: document.querySelector('#result-count'),
  activeStat: document.querySelector('#active-stat'),
  speedStat: document.querySelector('#speed-stat'),
  addDialog: document.querySelector('#add-dialog'),
  deleteDialog: document.querySelector('#delete-dialog'),
  confirmDelete: document.querySelector('#confirm-delete'),
  toastRegion: document.querySelector('#toast-region'),
  folderDialog: document.querySelector('#folder-dialog'),
  folderPathInput: document.querySelector('#folder-path-input'),
  folderBrowser: document.querySelector('#folder-browser'),
  folderSelection: document.querySelector('#folder-selection'),
  folderError: document.querySelector('#folder-error'),
  newFolderRow: document.querySelector('#new-folder-row'),
  newFolderName: document.querySelector('#new-folder-name'),
  newFolderCreate: document.querySelector('#new-folder-create'),
};

const icons = {
  file: '<svg viewBox="0 0 24 24"><path d="M6 3h8l4 4v14H6zM14 3v5h4M9 13h6m-6 4h4"/></svg>',
  pause: '<svg viewBox="0 0 24 24"><path d="M9 5v14m6-14v14"/></svg>',
  play: '<svg viewBox="0 0 24 24"><path d="m8 5 11 7-11 7z"/></svg>',
  download: '<svg viewBox="0 0 24 24"><path d="M12 4v11m0 0 4-4m-4 4-4-4M5 20h14"/></svg>',
  trash: '<svg viewBox="0 0 24 24"><path d="M4 7h16m-10 4v6m4-6v6M9 7l1-3h4l1 3m3 0-1 14H7L6 7"/></svg>',
  folder: '<svg viewBox="0 0 24 24"><path d="M3 7h7l2 2h9v10H3z"/></svg>',
  cloud: '<svg viewBox="0 0 24 24"><path d="M8 18H6a4 4 0 0 1-.4-8A6.5 6.5 0 0 1 18 9a4.5 4.5 0 0 1 0 9h-2M12 12v9m0 0 3-3m-3 3-3-3"/></svg>',
};

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
  });
  if (!response.ok) {
    let message = `Request failed (${response.status})`;
    try { message = (await response.json()).error || message; } catch (_) { /* response was not JSON */ }
    throw new Error(message);
  }
  return response.status === 204 ? null : response.json();
}

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>'"]/g, character => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;',
  })[character]);
}

function formatBytes(bytes, decimals = 1) {
  if (!Number.isFinite(bytes) || bytes < 0) return 'Unknown';
  if (bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / (1024 ** index);
  return `${value.toFixed(index === 0 ? 0 : decimals)} ${units[index]}`;
}

function progress(job) {
  if (job.status === 'completed') return 100;
  if (!job.total_bytes || job.total_bytes < 1) return 0;
  return Math.min(100, Math.max(0, (job.downloaded_bytes / job.total_bytes) * 100));
}

function statusLabel(status) {
  return { downloading: 'Downloading', queued: 'Queued', completed: 'Completed', paused: 'Paused', error: 'Failed' }[status] || status;
}

function metaText(job) {
  if (job.status === 'downloading') return `${formatBytes(job.downloaded_bytes)} of ${formatBytes(job.total_bytes)} · ${formatBytes(job.speed_bytes_per_second)}/s`;
  if (job.status === 'completed') return `${formatBytes(job.downloaded_bytes)} · ${job.destination}`;
  if (job.status === 'queued') return `Waiting for a download slot · ${job.destination}`;
  if (job.status === 'paused') return `${formatBytes(job.downloaded_bytes)} of ${formatBytes(job.total_bytes)} · ${job.destination}`;
  return `${formatBytes(job.downloaded_bytes)} · ${job.destination}`;
}

function actionButtons(job) {
  const busy = state.busy.has(job.id) ? 'disabled' : '';
  let primary = '';
  if (job.status === 'downloading' || job.status === 'queued') {
    primary = `<button class="icon-button action-primary" data-action="pause" data-id="${job.id}" aria-label="Pause ${escapeHTML(job.filename)}" title="Pause" ${busy}>${icons.pause}</button>`;
  } else if (job.status === 'paused' || job.status === 'error') {
    primary = `<button class="icon-button action-primary" data-action="resume" data-id="${job.id}" aria-label="Resume ${escapeHTML(job.filename)}" title="Resume" ${busy}>${icons.play}</button>`;
  } else if (job.status === 'completed') {
    primary = `<a class="icon-button action-primary" href="/api/downloads/${job.id}/file" aria-label="Save ${escapeHTML(job.filename)}" title="Save file">${icons.download}</a>`;
  }
  return `${primary}<button class="icon-button danger" data-action="delete" data-id="${job.id}" aria-label="Delete ${escapeHTML(job.filename)}" title="Delete">${icons.trash}</button>`;
}

function renderDownloads() {
  const filtered = state.downloads.filter(job => {
    if (state.filter === 'all') return true;
    if (state.filter === 'active') return job.status === 'downloading' || job.status === 'queued';
    return job.status === state.filter;
  });

  elements.resultCount.textContent = `${filtered.length} ${filtered.length === 1 ? 'file' : 'files'}`;
  const active = state.downloads.filter(job => job.status === 'downloading' || job.status === 'queued');
  elements.activeStat.textContent = active.length;
  elements.speedStat.textContent = `${formatBytes(active.reduce((sum, job) => sum + Math.max(job.speed_bytes_per_second, 0), 0))}/s`;

  if (!filtered.length) {
    const allEmpty = state.downloads.length === 0;
    elements.list.innerHTML = `<div class="empty-state"><div class="empty-icon">${icons.cloud}</div><h3>${allEmpty ? 'No downloads' : 'No downloads in this view'}</h3><p>${allEmpty ? 'Click Add to download a file.' : 'Choose another filter to see your files.'}</p></div>`;
    return;
  }

  elements.list.innerHTML = filtered.map(job => {
    const percent = progress(job);
    const displayPercent = job.total_bytes > 0 || job.status === 'completed' ? `${Math.round(percent)}%` : '—';
    return `<article class="download-card" data-job-id="${job.id}">
      <div class="download-main">
        <div class="file-line"><div class="file-icon">${icons.file}</div><div class="file-copy"><span class="file-name" title="${escapeHTML(job.filename)}">${escapeHTML(job.filename)}</span><span class="file-url" title="${escapeHTML(job.url)}">${escapeHTML(job.url)}</span></div></div>
        <div class="file-meta"><span class="status ${job.status}">${statusLabel(job.status)}</span><span class="meta-divider"></span><span>${escapeHTML(metaText(job))}</span></div>
        <div class="progress-row"><progress class="progress-track ${job.status}" max="100" value="${percent}" aria-label="${Math.round(percent)} percent downloaded">${displayPercent}</progress><span class="progress-label">${displayPercent}</span></div>
        ${job.error ? `<p class="error-message" title="${escapeHTML(job.error)}">${escapeHTML(job.error)}</p>` : ''}
      </div>
      <div class="download-actions">${actionButtons(job)}</div>
    </article>`;
  }).join('');
}

function renderDestination() {
  elements.folderPath.textContent = state.selectedPath;
  elements.folderPath.title = state.selectedPath;
}

function openAddDialog() {
  elements.error.textContent = '';
  elements.addDialog.showModal();
  requestAnimationFrame(() => elements.url.focus());
}

async function loadDirectory(path) {
  elements.folderError.textContent = '';
  elements.folderBrowser.innerHTML = '<div class="folder-loading">Loading folders…</div>';
  try {
    const listing = await api(`/api/filesystem?path=${encodeURIComponent(path || state.defaultPath)}`);
    state.browserPath = listing.path;
    state.browserParent = listing.parent;
    elements.folderPathInput.value = listing.path;
    elements.folderSelection.textContent = listing.path;
    elements.folderSelection.title = listing.path;
    elements.folderBrowser.innerHTML = listing.directories.length
      ? listing.directories.map(directory => `<button class="folder-row" type="button" data-path="${escapeHTML(directory.path)}">${icons.folder}<span>${escapeHTML(directory.name)}</span></button>`).join('')
      : '<div class="folder-empty">This folder has no subfolders.</div>';
  } catch (error) {
    elements.folderError.textContent = error.message;
    elements.folderBrowser.innerHTML = '<div class="folder-empty">This location could not be opened.</div>';
  }
}

function openFolderPicker() {
  hideNewFolder();
  elements.folderDialog.showModal();
  loadDirectory(state.selectedPath || state.defaultPath);
}

function chooseFolder() {
  if (!state.browserPath) return;
  state.selectedPath = state.browserPath;
  renderDestination();
  elements.folderDialog.close();
}

function showNewFolder() {
  elements.folderError.textContent = '';
  elements.newFolderRow.classList.remove('hidden');
  elements.newFolderName.value = '';
  elements.newFolderName.focus();
}

function hideNewFolder() {
  elements.newFolderRow.classList.add('hidden');
  elements.newFolderName.value = '';
}

async function createFolder() {
  const name = elements.newFolderName.value.trim();
  if (!name) {
    elements.folderError.textContent = 'Enter a folder name.';
    elements.newFolderName.focus();
    return;
  }
  elements.folderError.textContent = '';
  elements.newFolderCreate.disabled = true;
  try {
    const directory = await api('/api/filesystem', {
      method: 'POST',
      body: JSON.stringify({ parent: state.browserPath, name }),
    });
    state.selectedPath = directory.path;
    renderDestination();
    hideNewFolder();
    elements.folderDialog.close();
    toast(`Folder “${directory.name}” created and selected`);
  } catch (error) {
    elements.folderError.textContent = error.message;
  } finally {
    elements.newFolderCreate.disabled = false;
  }
}

async function refresh(silent = true) {
  try {
    const data = await api('/api/downloads');
    state.downloads = data.downloads || [];
    renderDownloads();
  } catch (error) {
    if (!silent) toast(error.message, true);
  }
}

async function createDownload(event) {
  event.preventDefault();
  elements.error.textContent = '';
  elements.submit.disabled = true;
  try {
    const job = await api('/api/downloads', {
      method: 'POST',
      body: JSON.stringify({ url: elements.url.value.trim(), destination: state.selectedPath }),
    });
    state.downloads.unshift(job);
    elements.url.value = '';
    elements.addDialog.close();
    renderDownloads();
    toast('Download added to the queue');
  } catch (error) {
    elements.error.textContent = error.message;
  } finally {
    elements.submit.disabled = false;
  }
}

async function runAction(action, id) {
  if (action === 'delete') {
    state.pendingDelete = id;
    elements.deleteDialog.showModal();
    return;
  }
  state.busy.add(id);
  renderDownloads();
  try {
    const updated = await api(`/api/downloads/${id}/${action}`, { method: 'POST', body: '{}' });
    state.downloads = state.downloads.map(job => job.id === id ? updated : job);
    toast(action === 'pause' ? 'Download paused' : 'Download resumed');
  } catch (error) {
    toast(error.message, true);
  } finally {
    state.busy.delete(id);
    renderDownloads();
  }
}

async function confirmDelete(event) {
  event.preventDefault();
  const id = state.pendingDelete;
  elements.deleteDialog.close();
  if (!id) return;
  try {
    await api(`/api/downloads/${id}`, { method: 'DELETE' });
    state.downloads = state.downloads.filter(job => job.id !== id);
    renderDownloads();
    toast('Download and file deleted');
  } catch (error) {
    toast(error.message, true);
  } finally {
    state.pendingDelete = null;
  }
}

function toast(message, isError = false) {
  const item = document.createElement('div');
  item.className = `toast${isError ? ' error' : ''}`;
  item.textContent = message;
  elements.toastRegion.append(item);
  setTimeout(() => item.remove(), 3500);
}

function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem('webcp-theme', theme);
}

function toggleTheme() {
  setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
}

async function initialize() {
  const savedTheme = localStorage.getItem('webcp-theme');
  if (savedTheme) setTheme(savedTheme);
  else if (matchMedia('(prefers-color-scheme: dark)').matches) setTheme('dark');

  elements.form.addEventListener('submit', createDownload);
  elements.list.addEventListener('click', event => {
    const button = event.target.closest('[data-action]');
    if (button) runAction(button.dataset.action, button.dataset.id);
  });
  elements.filters.addEventListener('click', event => {
    const button = event.target.closest('[data-filter]');
    if (!button) return;
    state.filter = button.dataset.filter;
    elements.filters.querySelectorAll('button').forEach(item => item.classList.toggle('active', item === button));
    renderDownloads();
  });
  document.querySelector('#theme-toggle').addEventListener('click', toggleTheme);
  document.querySelector('#open-add-dialog').addEventListener('click', openAddDialog);
  document.querySelector('#add-close').addEventListener('click', () => elements.addDialog.close());
  document.querySelector('#add-cancel').addEventListener('click', () => elements.addDialog.close());
  elements.confirmDelete.addEventListener('click', confirmDelete);
  elements.deleteDialog.addEventListener('close', () => { if (elements.deleteDialog.returnValue === 'cancel') state.pendingDelete = null; });
  elements.folderButton.addEventListener('click', openFolderPicker);
  document.querySelector('#folder-close').addEventListener('click', () => elements.folderDialog.close());
  document.querySelector('#folder-choose').addEventListener('click', chooseFolder);
  document.querySelector('#folder-up').addEventListener('click', () => loadDirectory(state.browserParent));
  document.querySelector('#folder-default').addEventListener('click', () => loadDirectory(state.defaultPath));
  document.querySelector('#folder-new').addEventListener('click', showNewFolder);
  document.querySelector('#new-folder-cancel').addEventListener('click', hideNewFolder);
  elements.newFolderCreate.addEventListener('click', createFolder);
  elements.newFolderName.addEventListener('keydown', event => {
    if (event.key === 'Enter') {
      event.preventDefault();
      createFolder();
    } else if (event.key === 'Escape') {
      hideNewFolder();
    }
  });
  document.querySelector('#folder-go').addEventListener('click', () => loadDirectory(elements.folderPathInput.value));
  elements.folderPathInput.addEventListener('keydown', event => {
    if (event.key === 'Enter') {
      event.preventDefault();
      loadDirectory(elements.folderPathInput.value);
    }
  });
  elements.folderBrowser.addEventListener('click', event => {
    const folder = event.target.closest('[data-path]');
    if (folder) loadDirectory(folder.dataset.path);
  });

  try {
    const config = await api('/api/config');
    state.defaultPath = config.default_path;
    state.selectedPath = config.default_path;
    renderDestination();
    await refresh(false);
  } catch (error) {
    toast(error.message, true);
  }
  setInterval(() => refresh(true), 1000);
}

initialize();
