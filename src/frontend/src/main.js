import './style.css';
import './app.css';
import { DetectGames, BrowseExe, GetGameFolderPreview, GetGameDetails, GetGameDetailsForExe, ListGameExes, PatchGameWithMode, PatchGameWithModeForExe, LaunchGame, GetAppVersion, UninstallPatch, KillGameProcess, GetGPUs, SelectGPU, RefreshGPUs, GetDefenderStatus, AddDefenderExclusion, RemoveDefenderExclusion, IsPathExcluded, GetDownloadURLs, SaveDownloadURLs, VerifyInstall, VerifyInstallForExe, RepairInstall, RepairInstallForExe, OpenGameFolder } from '../wailsjs/go/main/App';
import { EventsOn, BrowserOpenURL } from '../wailsjs/runtime/runtime';

document.querySelector('#app').innerHTML = `
  <div class="container">
    <header class="header">
      <button id="settingsBtn" class="settings-btn" title="Settings" aria-label="Settings">⚙</button>
      <h1>DLSS 5 Patcher</h1>
      <div class="gpu-preview" id="gpuPreview">
        <span class="gpu-preview-label">GPU:</span>
        <select id="gpuSelect" class="gpu-select" title="Select GPU for this machine">
          <option value="">Detecting...</option>
        </select>
        <span class="gpu-preview-badge" id="gpuNRBadge">NR: --</span>
      </div>
    </header>

    <main class="main">
      <section class="game-selection">
        <div class="section-header">
          <h2>Select Game</h2>
          <div class="header-controls">
            <div class="search-box">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                <circle cx="11" cy="11" r="8"></circle>
                <line x1="21" y1="21" x2="16.65" y2="16.65"></line>
              </svg>
              <input type="text" id="searchInput" placeholder="Search game..." aria-label="Search game">
              <button id="clearSearchBtn" class="clear-btn" style="display: none;" title="Clear search">&times;</button>
            </div>
            <button id="refreshBtn" class="btn-icon" title="Refresh game list" aria-label="Refresh game list">
              <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                <path d="M21.5 2v6h-6M2.5 22v-6h6M2 11.5a10 10 0 0 1 18.8-4.3M22 12.5a10 10 0 0 1-18.8 4.2"/>
              </svg>
            </button>
          </div>
        </div>
        <div class="game-list-container">
          <div id="gameList" class="game-list"></div>
        </div>
        <div id="scanStatusText" class="scan-status-text" style="display: none; margin-bottom: 20px;"></div>
        <div class="browse-section">
          <button id="browseBtn" class="btn btn-secondary">Browse Game Executable</button>
        </div>
      </section>

      <section class="preview-section" id="previewSection" style="display: none;">
        <h2>Game Preview</h2>
        <div class="folder-path" id="folderPath" style="display:none;"></div>
        <div class="folder-contents" id="folderContents"></div>
      </section>

      <section class="action-section">
        <div id="patchSection" style="display: none;">
          <div class="patch-mode-toggle">
            <label class="mode-label">Patch Method:</label>
            <div class="mode-options">
              <button id="modeOptiscaler" class="mode-btn mode-btn-active" data-mode="optiscaler">
                <span class="mode-btn-title">OptiScaler (FSR/XeSS/DLSS)</span>
                <span class="mode-recommended" id="modeOptiscalerRec">Recommended</span>
              </button>
              <button id="modeReshade" class="mode-btn" data-mode="reshade">
                <span class="mode-btn-title">ReShade + DLSS 5 Add-On</span>
                <span class="mode-recommended" id="modeReshadeRec">Recommended</span>
              </button>
            </div>
          </div>
          <div class="action-row">
            <button id="patchBtn" class="btn btn-primary btn-large">Patch Game</button>
            <button id="verifyBtn" class="btn btn-secondary btn-large" style="display: none;">Verify</button>
            <button id="uninstallBtn" class="btn btn-danger btn-large" style="display: none;">Uninstall</button>
          </div>
          <div id="patchProgress" style="display: none;">
            <div class="progress">
              <div class="progress-bar" id="progressBar"></div>
            </div>
            <div id="patchStatusText" class="patch-status-text"></div>
          </div>
          <div id="patchResult" class="result"></div>
        </div>
        <div id="launchSection" style="display: none;">
          <button id="launchBtn" class="btn btn-success btn-large">Launch Game</button>
        </div>
      </section>
    </main>

    <footer class="footer">
      <div class="footer-content">
        <p>Created by <a href="https://github.com/MRHRTZ" target="_blank" class="footer-link">MRHRTZ</a></p>
        <div class="footer-buttons">
          <button id="discordBtn" class="btn btn-discord">MNXMN Community</button>
        </div>
      </div>
    </footer>
  </div>

  <div id="customModal" class="modal-overlay" style="display: none;">
    <div class="modal-container">
      <div class="modal-header">
        <h3 id="modalTitle">Confirm Uninstall</h3>
        <button id="modalCloseBtn" class="modal-close-btn" aria-label="Close">&times;</button>
      </div>
      <div class="modal-body" id="modalBody"></div>
      <div class="modal-footer">
        <button id="modalCancelBtn" class="btn btn-secondary">Cancel</button>
        <button id="modalConfirmBtn" class="btn btn-danger">Uninstall</button>
      </div>
    </div>
  </div>

  <div id="settingsModal" class="modal-overlay" style="display: none;">
    <div class="modal-container settings-container">
      <div class="modal-header">
        <h3 class="settings-title">⚙ Settings</h3>
        <button id="settingsCloseBtn" class="modal-close-btn" aria-label="Close">&times;</button>
      </div>
      <div class="modal-body">
        <h4 class="settings-section-title">Download URLs</h4>
        <p class="settings-hint">Dataset zips are downloaded from these URLs when local files are missing. Leave a field empty to disable auto-download for it.</p>
        <label class="settings-label">ReShade Setup (.exe)
          <input type="text" id="urlReshadeSetup" class="settings-input" placeholder="https://..." spellcheck="false">
        </label>
        <label class="settings-label">ReShade shaders (.zip)
          <input type="text" id="urlReshade" class="settings-input" placeholder="https://..." spellcheck="false">
        </label>
        <label class="settings-label">OptiScaler (.zip)
          <input type="text" id="urlOptiscaler" class="settings-input" placeholder="https://..." spellcheck="false">
        </label>
        <label class="settings-label">DLSS 5 (.zip)
          <input type="text" id="urlDlss5" class="settings-input" placeholder="https://..." spellcheck="false">
        </label>
        <label class="settings-label">dgVoodoo2 (.zip)
          <input type="text" id="urlDgvoodoo" class="settings-input" placeholder="https://..." spellcheck="false">
        </label>
        <label class="settings-label">DLSS5-Feeder 32-bit (.zip)
          <input type="text" id="urlFeeder" class="settings-input" placeholder="https://..." spellcheck="false">
        </label>
        <div class="settings-row settings-tight">
          <label class="settings-label settings-inline-label" for="dgvoodooApi">dgVoodoo2 output</label>
          <select id="dgvoodooApi" class="exe-select">
            <option value="d3d11">Direct3D 11 (recommended)</option>
            <option value="d3d12">Direct3D 12</option>
          </select>
        </div>
        <p class="settings-hint">Bridge output for DX9 games — applies on the next patch (rewrites dgVoodoo.conf + matching ReShade hook).</p>
        <div class="settings-row settings-tight">
          <label class="settings-label settings-inline-label" for="neuralConsumer">Neural consumer</label>
          <select id="neuralConsumer" class="exe-select">
            <option value="renodx">RenoDX DLSS 5 (default)</option>
            <option value="dfc">Deep Fried Chicken</option>
          </select>
        </div>
        <p class="settings-hint">Which neural add-on to deploy — applies on the next patch (the other one is removed automatically).</p>
        <div class="settings-row">
          <button id="saveUrlsBtn" class="btn btn-primary btn-small">Save Settings</button>
          <span id="urlsSaveResult" class="settings-result"></span>
        </div>
        <h4 class="settings-section-title">Windows Defender</h4>
        <p class="settings-hint">dgVoodoo2 is often false-flagged by Defender and quarantined. Exclude the app folder (wherever you downloaded it) so datasets survive. A Windows permission prompt will appear — that is normal.</p>
        <div class="settings-row">
          <span id="defenderStatusText" class="settings-status">Checking...</span>
        </div>
        <div class="settings-row">
          <code id="defenderAppDir" class="settings-path"></code>
        </div>
        <div class="settings-row">
          <button id="excludeAppBtn" class="btn btn-primary btn-compact">Exclude App Folder</button>
        </div>
        <div id="defenderResult" class="settings-result"></div>
      </div>
    </div>
  </div>
`;

let selectedGame = null;
let exeChoiceByGame = {};
let games = [];
let isScanning = false;
let searchQuery = '';
let patchMode = 'optiscaler';
let modeManuallySet = false;
let reshadeRecommended = false;

// Normalize game object to have consistent lowercase property keys
function normalizeGame(game) {
  if (!game) return null;
  return {
    name: game.name || game.Name || 'Unknown Game',
    path: game.path || game.Path || '',
    exePath: game.exePath || game.ExePath || '',
    isInstalled: !!(game.isInstalled ?? game.IsInstalled),
    detectedAPI: (game.detectedAPI || game.DetectedAPI || 'dxgi').toLowerCase()
  };
}

// Wails Event Listeners for live scanning
EventsOn('scan:status', (msg) => {
  const elem = document.getElementById('scanStatusText');
  if (!elem) return;
  if (msg && msg.trim()) {
    elem.style.display = 'block';
    elem.textContent = msg;
  } else {
    elem.style.display = 'none';
    elem.textContent = '';
  }
});

EventsOn('scan:game', (rawGame) => {
  const game = normalizeGame(rawGame);
  if (!game || !game.path) return;
  const exists = games.some(g => g.path.toLowerCase() === game.path.toLowerCase());
  if (!exists) {
    games.push(game);
    renderGameList();
  }
});

EventsOn('scan:complete', () => {
  isScanning = false;
  const elem = document.getElementById('scanStatusText');
  if (elem) {
    elem.style.display = 'none';
    elem.textContent = '';
  }
  const refreshBtn = document.getElementById('refreshBtn');
  if (refreshBtn) {
    refreshBtn.disabled = false;
    refreshBtn.classList.remove('spinning');
  }
});

// Live progress text from the backend during patch / uninstall
EventsOn('patch:status', (msg) => {
  const el = document.getElementById('patchStatusText');
  if (el) {
    el.textContent = msg && msg.trim() ? msg : '';
  }
});

// Load games on startup sequentially with real-time UI updates
async function loadGames() {
  games = [];
  isScanning = true;
  const gameList = document.getElementById('gameList');
  gameList.innerHTML = '<div class="loading">Searching for installed games...</div>';

  try {
    const rawGames = await DetectGames();
    if (rawGames && rawGames.length > 0) {
      rawGames.forEach(rg => {
        const game = normalizeGame(rg);
        if (game && game.path && !games.some(g => g.path.toLowerCase() === game.path.toLowerCase())) {
          games.push(game);
        }
      });
    }
    renderGameList();
  } catch (err) {
    console.error('Failed to detect games:', err);
    gameList.innerHTML = '<div class="error">Failed to detect games. Click "Browse Game Executable" to select manually.</div>';
  } finally {
    isScanning = false;
    const elem = document.getElementById('scanStatusText');
    if (elem) {
      elem.style.display = 'none';
      elem.textContent = '';
    }
    const refreshBtn = document.getElementById('refreshBtn');
    if (refreshBtn) {
      refreshBtn.disabled = false;
      refreshBtn.classList.remove('spinning');
    }
  }
}

function renderGameList() {
  const gameList = document.getElementById('gameList');

  const filteredGames = games.filter(g => {
    if (!searchQuery) return true;
    return (g.name && g.name.toLowerCase().includes(searchQuery)) ||
           (g.path && g.path.toLowerCase().includes(searchQuery));
  });

  if (!filteredGames || filteredGames.length === 0) {
    if (searchQuery) {
      gameList.innerHTML = `<div class="empty">No games match "${searchQuery}"</div>`;
    } else {
      gameList.innerHTML = '<div class="empty">No games found. Click "Browse Game Executable" to select manually.</div>';
    }
    return;
  }

  gameList.innerHTML = filteredGames.map((game) => {
    const originalIndex = games.findIndex(g => g.path.toLowerCase() === game.path.toLowerCase());
    const isSelected = selectedGame && selectedGame.path.toLowerCase() === game.path.toLowerCase();
    return `
      <div class="game-item ${game.isInstalled ? 'installed' : ''} ${isSelected ? 'selected' : ''}" data-index="${originalIndex}">
        <div class="game-info">
          <div class="game-header">
            <span class="game-name">${game.name}</span>
            <span class="api-badge">⚙️ ${game.detectedAPI.toUpperCase()}</span>
          </div>
          <span class="game-path">${game.path}</span>
          ${game.isInstalled ? '<span class="badge badge-success">DLSS 5 Installed</span>' : '<span class="badge badge-warning">Not Patched</span>'}
        </div>
      </div>
    `;
  }).join('');

  document.querySelectorAll('.game-item').forEach(item => {
    item.addEventListener('click', () => {
      const index = parseInt(item.dataset.index);
      if (games[index]) {
        selectGame(games[index]);
      }
    });
  });
}

async function selectGame(game) {
  if (!game) return;
  const previousPath = selectedGame ? selectedGame.path : null;
  selectedGame = normalizeGame(game);

  if (!previousPath || previousPath.toLowerCase() !== selectedGame.path.toLowerCase()) {
    clearResult();
  }

  document.querySelectorAll('.game-item').forEach(item => {
    const index = parseInt(item.dataset.index);
    if (games[index] && games[index].path.toLowerCase() === selectedGame.path.toLowerCase()) {
      item.classList.add('selected');
    } else {
      item.classList.remove('selected');
    }
  });

  const previewSection = document.getElementById('previewSection');
  const folderContents = document.getElementById('folderContents');

  previewSection.style.display = 'block';
  folderContents.innerHTML = `<div class="loading">Loading component details...</div>`;

  let gameDetails = null;
  try {
    const choiceKey = (selectedGame.path || '').toLowerCase();
    let availableExes = [];
    try {
      availableExes = await ListGameExes(selectedGame.path || selectedGame.exePath) || [];
    } catch (e) {
      console.error('Failed to list game exes:', e);
    }
    let manualExe = exeChoiceByGame[choiceKey] || '';
    if (manualExe && !availableExes.some(e => ((e.path || e.Path) || '').toLowerCase() === manualExe.toLowerCase())) {
      manualExe = '';
      delete exeChoiceByGame[choiceKey];
    }
    const details = manualExe
      ? await GetGameDetailsForExe(selectedGame.path, manualExe)
      : await GetGameDetails(selectedGame.path || selectedGame.exePath);
    gameDetails = details;

    if (details && details.isInstalled !== undefined) {
      selectedGame.isInstalled = !!details.isInstalled;
    }

    // The backend explicitly flags APIs OptiScaler cannot handle (D3D8/D3D9/
    // D3D10/OpenGL, where its proxy would never load); the old neuralNote
    // heuristic stays as fallback.
    reshadeRecommended = !!details.recommendsReShade ||
      !!(details.neuralNote && details.neuralNote.indexOf('ReShade method') !== -1);
    applyRecommendedMode();

    const currentExe = manualExe
      || (((availableExes.find(e => !!(e.isTarget ?? e.IsTarget)) || {}).path) || ((availableExes[0] || {}).path) || '');

    const initial = (details.name || selectedGame.name || 'G').charAt(0).toUpperCase();

    folderContents.innerHTML = `
      <div class="game-details-card">
        <div class="details-header">
          ${details.coverArt ? `
            <div class="game-cover"><img class="game-cover-img" src="${details.coverArt}" alt="${details.name || selectedGame.name}" onerror="this.closest('.game-cover').remove();"></div>
          ` : `<div class="game-avatar">${initial}</div>`}
          <div class="game-title-group">
            <h3 class="game-title">${details.name || selectedGame.name}<span class="bit-badge ${details.is32Bit ? 'bit32' : 'bit64'}">${details.is32Bit ? '32-bit' : '64-bit'}</span></h3>
            <div class="game-subtitle">Game folder (click to open in Explorer):</div>
            <div class="game-folder-path clickable" id="gameFolderPath" title="Open in Explorer">${details.path || selectedGame.path}</div>
          </div>
          <button id="excludeGameBtn" class="btn btn-secondary btn-compact details-exclude-btn" title="Exclude this game's folder from Windows Defender so wrapper/hook files are not quarantined">🛡</button>
        </div>

        <div class="details-table">
          <div class="details-row">
            <span class="row-label">Executable</span>
            ${availableExes.length > 1 ? `
            <select id="exeSelect" class="exe-select" title="Target executable — change it if auto-detection picked the wrong one">
              <option value="">Auto-detect (recommended)</option>
              ${availableExes.map(e => {
                const p = e.path || e.Path || '';
                const nm = e.name || e.Name || p;
                const api = ((e.api || e.API) || '').toUpperCase();
                const sz = Number(e.size ?? e.Size ?? 0);
                const sizeTxt = sz > 0 ? ' — ' + (sz / 1048576).toFixed(1) + ' MB' : '';
                const isT = !!(e.isTarget ?? e.IsTarget);
                const sel = (currentExe && p.toLowerCase() === currentExe.toLowerCase()) ? ' selected' : '';
                return `<option value="${escapeHtml(p)}"${sel}>${escapeHtml(nm)}${sizeTxt}${api ? ' · ' + escapeHtml(api) : ''}${isT ? ' ★' : ''}</option>`;
              }).join('')}
            </select>` : `<span class="row-value exe-value">${details.executable || selectedGame.exePath}</span>`}
          </div>
          <div class="details-row">
            <span class="row-label">Rendering API</span>
            <span class="row-value api-value">${details.renderingAPI || 'DXGI (DirectX 11/12)'}</span>
          </div>
          <div class="details-row">
            <span class="row-label">DLSS</span>
            <span class="row-value ${details.dlssVersion !== 'Not Available' ? 'status-green' : 'status-muted'}">${details.dlssVersion || 'Not Available'}</span>
          </div>
          <div class="details-row">
            <span class="row-label">DLSS 5 add-on</span>
            <span class="row-value ${(details.dlss5Addon || '').indexOf('Installed') === 0 ? 'status-green' : 'status-muted'}">${details.dlss5Addon || 'Not Installed'}</span>
          </div>
          <div class="details-row">
            <span class="row-label">ReShade</span>
            <span class="row-value ${details.reshadeStatus !== 'Not Installed' ? 'status-green' : 'status-muted'}">${details.reshadeStatus || 'Not Installed'}</span>
          </div>
          <div class="details-row">
            <span class="row-label">OptiScaler</span>
            <span class="row-value ${details.optiScalerStatus !== 'Not Installed' ? 'status-green' : 'status-muted'}">${details.optiScalerStatus || 'Not Installed'}</span>
          </div>
          <div class="details-row">
            <span class="row-label">dgVoodoo2</span>
            <span class="row-value ${details.dgvoodooStatus && details.dgvoodooStatus !== 'Not Installed' ? 'status-green' : 'status-muted'}">${details.dgvoodooStatus || 'Not Installed'}</span>
          </div>
          ${details.gpuName ? `
          <div class="details-row">
            <span class="row-label">GPU</span>
            <span class="row-value api-value">${details.gpuName}</span>
          </div>
          ` : ''}
        </div>

        ${details.neuralNote ? `
          <div class="neural-warning ${details.neuralNoteLevel === 'info' ? 'neural-info' : ''}">
            <span class="neural-warning-icon">${details.neuralNoteLevel === 'info' ? '&#9432;' : '&#9888;'}</span>
            <span>${details.neuralNote}</span>
          </div>
        ` : ''}

        ${details.dllList && details.dllList.length > 0 ? `
          <div class="dll-table-container">
            <div class="dll-table-header">Detected Streamline & DLSS Files</div>
            <div class="dll-list-scroll">
              ${details.dllList.map(dll => `
                <div class="dll-row">
                  <span class="dll-path">${dll.relPath}</span>
                  <span class="dll-version">${dll.version || 'Unknown'}</span>
                </div>
              `).join('')}
            </div>
          </div>
        ` : ''}
      </div>
    `;

    document.getElementById('exeSelect')?.addEventListener('change', (e) => {
      if (!selectedGame || !selectedGame.path) return;
      const k = selectedGame.path.toLowerCase();
      if (e.target.value) exeChoiceByGame[k] = e.target.value;
      else delete exeChoiceByGame[k];
      selectGame(selectedGame);
    });
    // The header button is re-created on every render, so (re)attach here.
    document.getElementById('excludeGameBtn')?.addEventListener('click', handleExcludeGame);
    document.getElementById('gameFolderPath')?.addEventListener('click', handleOpenFolder);
    updateExcludeBtnState();
  } catch (err) {
    console.error('Failed to get game details:', err);
    folderContents.innerHTML = `
      <div class="folder-item">📁 <strong>Folder:</strong> ${selectedGame.path || 'Unknown path'}</div>
      <div class="folder-item">🎮 <strong>Executable:</strong> ${selectedGame.exePath || 'Unknown executable'}</div>
    `;
  }

  document.getElementById('patchSection').style.display = 'block';
  document.getElementById('launchSection').style.display = 'block';

  const uninstallBtn = document.getElementById('uninstallBtn');
  const verifyBtn = document.getElementById('verifyBtn');

  // Show Uninstall whenever patcher-owned files are present — not just on
  // the DLSS marker. A ReShade/dgVoodoo-only state (e.g. after a skipped or
  // failed DLSS step) must still be removable. Game-shipped "(built-in)"
  // ReShade builds never count.
  const rdStatus = (gameDetails && gameDetails.reshadeStatus) || '';
  const isPatcherReshade = rdStatus.indexOf('Installed') === 0 && rdStatus.indexOf('built-in') === -1;
  const isPatcherDgvoodoo = gameDetails && gameDetails.dgvoodooStatus && gameDetails.dgvoodooStatus !== 'Not Installed';
  const canUninstall = selectedGame.isInstalled || isPatcherReshade || isPatcherDgvoodoo;
  if (canUninstall) {
    uninstallBtn.style.display = 'block';
  } else {
    uninstallBtn.style.display = 'none';
  }
  // Verify is meaningful once patcher files exist; otherwise it only reports
  // "not installed". Same visibility as Uninstall.
  if (verifyBtn) {
    verifyBtn.style.display = canUninstall ? 'block' : 'none';
  }
  updatePatchButton();
  document.getElementById('previewSection').scrollIntoView({ behavior: 'smooth', block: 'start' });
}

async function handleBrowseExe() {
  try {
    const exePath = await BrowseExe();
    if (!exePath) return;

    const gameInfo = await GetGameFolderPreview(exePath);
    const normalized = normalizeGame(gameInfo);
    if (!normalized.exePath) normalized.exePath = exePath;

    const existingIndex = games.findIndex(g => g.path.toLowerCase() === normalized.path.toLowerCase());
    if (existingIndex !== -1) {
      games[existingIndex] = normalized;
    } else {
      games.unshift(normalized);
    }

    renderGameList();
    selectGame(normalized);
  } catch (err) {
    console.error('Browse failed:', err);
    showResult('error', 'Failed to browse for executable');
  }
}

async function handlePatch() {
  if (!selectedGame || !selectedGame.path) {
    showResult('error', 'Please select a game first');
    return;
  }

  const patchBtn = document.getElementById('patchBtn');
  const patchProgress = document.getElementById('patchProgress');
  const progressBar = document.getElementById('progressBar');
  const patchResult = document.getElementById('patchResult');

  patchBtn.disabled = true;
  patchBtn.textContent = 'Patching...';
  patchProgress.style.display = 'block';
  progressBar.style.width = '0%';
  patchResult.innerHTML = '';
  const statusEl = document.getElementById('patchStatusText');
  if (statusEl) statusEl.textContent = 'Starting patch...';

  let progress = 0;
  const progressInterval = setInterval(() => {
    progress += Math.random() * 12;
    if (progress > 90) progress = 90;
    progressBar.style.width = progress + '%';
  }, 200);

  try {
    const manualExe = exeChoiceByGame[(selectedGame.path || '').toLowerCase()] || '';
    const result = manualExe
      ? await PatchGameWithModeForExe(selectedGame.path, patchMode, manualExe)
      : await PatchGameWithMode(selectedGame.path, patchMode);
    clearInterval(progressInterval);
    progressBar.style.width = '100%';

    if (result.success || result.Success) {
      showResult('success', result.message || result.Message);
      selectedGame.isInstalled = true;
      
      const index = games.findIndex(g => g.path.toLowerCase() === selectedGame.path.toLowerCase());
      if (index !== -1) {
        games[index].isInstalled = true;
      }
      
      renderGameList();
      selectGame(selectedGame);
    } else {
      const msg = result.message || result.Message;
      if (isRunningBlocked(msg)) {
        showRunningBlocked(msg, selectedGame.path, handlePatch);
      } else {
        showResult('error', msg);
      }
    }
  } catch (err) {
    clearInterval(progressInterval);
    showResult('error', 'Patch failed: ' + err);
  } finally {
    patchBtn.disabled = false;
    updatePatchButton();
    setTimeout(() => {
      patchProgress.style.display = 'none';
      if (statusEl) statusEl.textContent = '';
    }, 1000);
  }
}

async function handleLaunch() {
  const manualExe = selectedGame && (exeChoiceByGame[(selectedGame.path || '').toLowerCase()] || '');
  const launchPath = manualExe || (selectedGame && selectedGame.exePath) || '';
  if (!selectedGame || !launchPath) {
    showResult('error', 'Game executable path not found');
    return;
  }

  const launchBtn = document.getElementById('launchBtn');
  launchBtn.disabled = true;
  launchBtn.textContent = 'Launching...';

  try {
    const result = await LaunchGame(launchPath);
    if (result.success || result.Success) {
      showResult('success', result.message || result.Message);
    } else {
      showResult('error', result.message || result.Message);
    }
  } catch (err) {
    showResult('error', 'Launch failed: ' + err);
  } finally {
    launchBtn.disabled = false;
    launchBtn.textContent = 'Launch Game';
  }
}

function renderVerifyReport(report) {
  const checks = report.checks || report.Checks || [];
  const autoFixed = report.autoFixed || report.AutoFixed || [];
  const summary = report.summary || report.Summary || '';
  const success = !!(report.success ?? report.Success);
  const rows = checks.map((c) => {
    const name = c.name || c.Name || '';
    const status = c.status || c.Status || '';
    const detail = c.detail || c.Detail || '';
    const fix = c.fix || c.Fix || '';
    const badge = status === 'ok' ? 'status-green' : (status === 'warn' ? 'status-warn' : 'status-red');
    const glyph = status === 'ok' ? '[ OK ]' : (status === 'warn' ? '[WARN]' : '[FAIL]');
    return `<div class="details-row"><span class="row-label ${badge}">${glyph}</span><span class="row-value"><strong>${escapeHtml(name)}</strong>${detail ? `<br><span class="status-muted">${escapeHtml(detail)}</span>` : ''}${fix ? `<br><span>→ ${escapeHtml(fix)}</span>` : ''}</span></div>`;
  }).join('');
  // NOTE: showResult() escapes everything via escapeHtml, so the check table
  // must be rendered here directly — otherwise the HTML shows up as raw text.
  const patchResult = document.getElementById('patchResult');
  patchResult.className = `result ${success ? 'success' : 'error'}`;
  patchResult.innerHTML = `
    <button class="result-close-btn" aria-label="Close">&times;</button>
    <div class="result-text">${escapeHtml(summary)}${autoFixed.length ? `<br>Auto-fixed: ${escapeHtml(autoFixed.join('; '))}` : ''}</div>
    <div class="details-table verify-table" style="margin-top:8px;">${rows}</div>
  `;
  const closeBtn = patchResult.querySelector('.result-close-btn');
  if (closeBtn) {
    closeBtn.addEventListener('click', () => {
      patchResult.className = 'result';
      patchResult.innerHTML = '';
    });
  }
}

async function handleVerify() {
  if (!selectedGame || !selectedGame.path) {
    showResult('error', 'Please select a game first');
    return;
  }
  const btn = document.getElementById('verifyBtn');
  if (btn) { btn.disabled = true; btn.textContent = 'Verifying...'; }
  try {
    const manualExe = exeChoiceByGame[(selectedGame.path || '').toLowerCase()] || '';
    const report = manualExe
      ? await VerifyInstallForExe(selectedGame.path, manualExe)
      : await VerifyInstall(selectedGame.path);
    renderVerifyReport(report);
    if (!(report.success ?? report.Success)) {
      const fails = (report.checks || report.Checks || []).filter((c) => (c.status || c.Status) === 'fail');
      const doRepair = await showConfirmModal({
        title: '⚠️ Verify found failures',
        bodyHtml: `<p>Verify found <strong>${fails.length} failing check(s)</strong>. Missing files can be repaired automatically (driver updates and similar user steps cannot).</p><p>Run <strong>Repair</strong> now?</p>`,
        confirmText: 'Repair Now',
        cancelText: 'Later'
      });
      if (doRepair) {
        await handleRepair();
      }
    }
  } catch (err) {
    showResult('error', 'Verify failed: ' + err);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Verify'; }
  }
}

async function handleRepair() {
  if (!selectedGame || !selectedGame.path) {
    showResult('error', 'Please select a game first');
    return;
  }
  const btn = document.getElementById('verifyBtn');
  if (btn) { btn.disabled = true; btn.textContent = 'Repairing...'; }
  try {
    const manualExe = exeChoiceByGame[(selectedGame.path || '').toLowerCase()] || '';
    const report = manualExe
      ? await RepairInstallForExe(selectedGame.path, manualExe)
      : await RepairInstall(selectedGame.path);
    renderVerifyReport(report);
    selectGame(selectedGame);
  } catch (err) {
    showResult('error', 'Repair failed: ' + err);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Verify'; }
  }
}

function showConfirmModal({ title = 'Confirm Uninstall', bodyHtml = '', confirmText = 'Uninstall', cancelText = 'Cancel' } = {}) {  return new Promise((resolve) => {
    const modal = document.getElementById('customModal');
    const modalTitle = document.getElementById('modalTitle');
    const modalBody = document.getElementById('modalBody');
    const modalConfirmBtn = document.getElementById('modalConfirmBtn');
    const modalCancelBtn = document.getElementById('modalCancelBtn');
    const modalCloseBtn = document.getElementById('modalCloseBtn');

    if (!modal) return resolve(false);

    modalTitle.innerHTML = title;
    modalBody.innerHTML = bodyHtml;
    modalConfirmBtn.textContent = confirmText;
    modalCancelBtn.textContent = cancelText;

    modal.style.display = 'flex';

    function cleanup(result) {
      modal.style.display = 'none';
      modalConfirmBtn.removeEventListener('click', onConfirm);
      modalCancelBtn.removeEventListener('click', onCancel);
      modalCloseBtn.removeEventListener('click', onCancel);
      modal.removeEventListener('click', onOverlayClick);
      resolve(result);
    }

    function onConfirm() { cleanup(true); }
    function onCancel() { cleanup(false); }
    function onOverlayClick(e) {
      if (e.target === modal) cleanup(false);
    }

    modalConfirmBtn.addEventListener('click', onConfirm);
    modalCancelBtn.addEventListener('click', onCancel);
    modalCloseBtn.addEventListener('click', onCancel);
    modal.addEventListener('click', onOverlayClick);
  });
}

async function handleUninstall() {
  if (!selectedGame || !selectedGame.path) {
    showResult('error', 'Please select a game first');
    return;
  }

  const confirmed = await showConfirmModal({
    title: '⚠️ Uninstall DLSS 5 & ReShade',
    bodyHtml: `
      <p>Are you sure you want to uninstall ReShade and DLSS 5 from <strong>${selectedGame.name}</strong>?</p>
      <p>This will remove:</p>
      <ul>
        <li>ReShade DLL files (from root & binary folders)</li>
        <li>DLSS 5 addon & DLL files</li>
        <li>Shader & preset files</li>
      </ul>
      <p>Original backed up files will be restored automatically.</p>
    `,
    confirmText: 'Uninstall Patch',
    cancelText: 'Cancel'
  });

  if (!confirmed) {
    return;
  }

  const uninstallBtn = document.getElementById('uninstallBtn');
  const patchProgress = document.getElementById('patchProgress');
  const progressBar = document.getElementById('progressBar');
  const patchResult = document.getElementById('patchResult');

  uninstallBtn.disabled = true;
  uninstallBtn.textContent = 'Uninstalling...';
  patchProgress.style.display = 'block';
  progressBar.style.width = '0%';
  patchResult.innerHTML = '';
  const statusEl = document.getElementById('patchStatusText');
  if (statusEl) statusEl.textContent = 'Starting uninstall...';

  let progress = 0;
  const progressInterval = setInterval(() => {
    progress += Math.random() * 15;
    if (progress > 90) progress = 90;
    progressBar.style.width = progress + '%';
  }, 150);

  try {
    const result = await UninstallPatch(selectedGame.path);
    clearInterval(progressInterval);
    progressBar.style.width = '100%';

    if (result.success || result.Success) {
      showResult('success', result.message || result.Message);
      selectedGame.isInstalled = false;
      
      const index = games.findIndex(g => g.path.toLowerCase() === selectedGame.path.toLowerCase());
      if (index !== -1) {
        games[index].isInstalled = false;
      }
      
      renderGameList();
      selectGame(selectedGame);
    } else {
      const msg = result.message || result.Message;
      if (isRunningBlocked(msg)) {
        showRunningBlocked(msg, selectedGame.path, handleUninstall);
      } else {
        showResult('error', msg);
      }
    }
  } catch (err) {
    clearInterval(progressInterval);
    showResult('error', 'Uninstall failed: ' + err);
  } finally {
    uninstallBtn.disabled = false;
    uninstallBtn.textContent = 'Uninstall Patch';
    setTimeout(() => {
      patchProgress.style.display = 'none';
      if (statusEl) statusEl.textContent = '';
    }, 1000);
  }
}

function showResult(type, message) {
  const patchResult = document.getElementById('patchResult');
  patchResult.className = `result ${type}`;
  const escaped = escapeHtml(message);
  patchResult.innerHTML = `
    <button class="result-close-btn" aria-label="Close">&times;</button>
    <div class="result-text">${escaped}</div>
  `;
  const closeBtn = patchResult.querySelector('.result-close-btn');
  if (closeBtn) {
    closeBtn.addEventListener('click', () => {
      patchResult.className = 'result';
      patchResult.innerHTML = '';
    });
  }
}

function clearResult() {
  const patchResult = document.getElementById('patchResult');
  if (patchResult) {
    patchResult.className = 'result';
    patchResult.innerHTML = '';
  }
}

function isRunningBlocked(message) {
  return typeof message === 'string' && message.toLowerCase().includes('currently running');
}

function escapeHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

async function endTaskAndRetry(gamePath, retryFn, btn) {
  if (!gamePath || !retryFn) return;
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'Ending task...';
  }
  try {
    const killed = await KillGameProcess(gamePath);
    showResult('success', `Ended ${killed}. Retrying...`);
  } catch (err) {
    showResult('error', String(err));
    return;
  }
  setTimeout(() => retryFn(),800);
}

function showRunningBlocked(message, gamePath, retryFn) {
  const patchResult = document.getElementById('patchResult');
  patchResult.className = 'result error';
  patchResult.innerHTML = `
    <div>${escapeHtml(message)}</div>
    <button id="endTaskBtn" class="btn btn-danger btn-small" style="margin-top: 6px;">End Task &amp; Try Again</button>
  `;
  document.getElementById('endTaskBtn').addEventListener('click', (e) => {
    endTaskAndRetry(gamePath, retryFn, e.currentTarget);
  });
}

// Search input handling
document.getElementById('searchInput')?.addEventListener('input', (e) => {
  searchQuery = e.target.value.trim().toLowerCase();
  const clearBtn = document.getElementById('clearSearchBtn');
  if (clearBtn) {
    clearBtn.style.display = searchQuery ? 'block' : 'none';
  }
  renderGameList();
});

document.getElementById('clearSearchBtn')?.addEventListener('click', () => {
  const searchInput = document.getElementById('searchInput');
  if (searchInput) searchInput.value = '';
  searchQuery = '';
  const clearBtn = document.getElementById('clearSearchBtn');
  if (clearBtn) clearBtn.style.display = 'none';
  renderGameList();
});

// Load version
async function loadVersion() {
  try {
    const version = await GetAppVersion();
    const versionElem = document.getElementById('version');
    if (versionElem) {
      versionElem.textContent = version;
    }
  } catch (err) {
    console.error('Failed to get version:', err);
  }
}

async function loadGpuInfo() {
  try {
    const gpus = await GetGPUs();
    renderGpuSelect(gpus);
  } catch (err) {
    console.error('Failed to get GPU info:', err);
    const sel = document.getElementById('gpuSelect');
    if (sel) {
      sel.replaceChildren(new Option('Unknown GPU', ''));
    }
    updateNRBadge(document.getElementById('gpuNRBadge'), null);
  }
}

function renderGpuSelect(gpus) {
  const sel = document.getElementById('gpuSelect');
  const nrBadgeElem = document.getElementById('gpuNRBadge');
  const preview = document.getElementById('gpuPreview');

  if (!sel) return;

  const options = gpus.map(g => {
    const opt = document.createElement('option');
    opt.value = g.name || '';
    let label = g.name || 'Unknown GPU';
    if (g.vendor) label = g.vendor + ' | ' + label;
    const tier = nrTierLabel(g);
    if (tier) label += ' [' + tier + ']';
    opt.textContent = label;
    if (g.selected || g.active) opt.textContent += ' (active)';
    return opt;
  });

  if (options.length === 0) {
    options.push(new Option('Unknown GPU', ''));
  }
  sel.replaceChildren(...options);

  const active = gpus.find(g => g.active) || gpus.find(g => g.selected) || gpus[0];
  if (active) {
    sel.value = active.name || '';
    updateNRBadge(nrBadgeElem, active);
  } else {
    updateNRBadge(nrBadgeElem, null);
  }

  // No game context yet during GPU load, so default recommendation is OptiScaler.
  reshadeRecommended = false;
  applyRecommendedMode();

  // Default to OptiScaler (always recommended) unless user has manually chosen.
  if (!modeManuallySet && patchMode !== 'optiscaler') {
    setPatchMode('optiscaler');
  }

  if (preview) preview.style.display = 'flex';
}

// applyRecommendedMode highlights the patch method best suited to the game AND,
// when the user has not explicitly chosen a method, auto-selects that method.
// Default (cross-vendor safe) is OptiScaler; for DirectX 11 games where
// OptiScaler's Neural Rendering needs DirectX 12, ReShade is recommended.
function applyRecommendedMode() {
  const recReshade = document.getElementById('modeReshadeRec');
  const recOpti = document.getElementById('modeOptiscalerRec');
  if (reshadeRecommended) {
    recOpti.style.display = 'none';
    recReshade.style.display = 'inline-flex';
    if (!modeManuallySet && patchMode !== 'reshade') {
      setPatchMode('reshade');
    }
  } else {
    recReshade.style.display = 'none';
    recOpti.style.display = 'inline-flex';
    if (!modeManuallySet && patchMode !== 'optiscaler') {
      setPatchMode('optiscaler');
    }
  }
}

function updateNRBadge(elem, gpu) {
  if (!elem) return;
  elem.classList.remove('badge-yes', 'badge-no', 'badge-unknown');
  if (!gpu) {
    elem.textContent = 'NR: --';
    elem.classList.add('badge-unknown');
    return;
  }
  const nr = !!(gpu.supportsNeuralRendering ?? gpu.SupportsNeuralRendering);
  if (nr) {
    const tier = nrTierLabel(gpu);
    elem.textContent = 'NR: Yes' + (tier ? ' (' + tier + ')' : '');
    elem.classList.add('badge-yes');
  } else {
    elem.textContent = 'NR: No';
    elem.classList.add('badge-no');
  }
}

// nrTierLabel maps a GPU's nrTier field to a short human-readable label.
function nrTierLabel(gpu) {
  if (!gpu) return '';
  const tier = String(gpu.nrTier != null ? gpu.nrTier : gpu.NRTier || '');
  switch (tier) {
    case 'rtx20-30': return 'RTX 20-30';
    case 'rtx40-50': return 'RTX 40-50';
    case 'none': return 'no NR';
    default: return '';
  }
}

function setupGpuControls() {
  const sel = document.getElementById('gpuSelect');

  if (!sel) return;

  // Refresh the GPU list every time the dropdown is opened so detection is
  // always current (replaces the old refresh button).
  sel.addEventListener('mousedown', async () => {
    // Avoid re-triggering while the list is already being refreshed.
    if (sel.dataset.refreshing === 'true') return;
    sel.dataset.refreshing = 'true';
    try {
      const fresh = await RefreshGPUs();
      renderGpuSelect(fresh);
    } catch (err) {
      console.error('Failed to refresh GPU list:', err);
    } finally {
      delete sel.dataset.refreshing;
    }
  });

  sel.addEventListener('change', async () => {
    const name = sel.value;
    try {
      const active = await SelectGPU(name);
      if (active) {
        refreshGpuBadge();
      }
    } catch (err) {
      console.error('Failed to select GPU:', err);
    }
    // The NR warning/info depends on the GPU, so re-evaluate the currently selected
    // game to refresh its preview and the ReShade/OptiScaler recommendation.
    if (selectedGame && selectedGame.path) {
      await selectGame(selectedGame);
    }
  });
}

async function refreshGpuBadge() {
  try {
    const gpus = await GetGPUs();
    const active = gpus.find(g => g.active) || gpus[0] || null;
    updateNRBadge(document.getElementById('gpuNRBadge'), active);
    const sel = document.getElementById('gpuSelect');
    if (active && sel) sel.value = active.name || '';
  } catch (err) {
    console.error('Failed to refresh GPU badge:', err);
  }
}

// ---- Settings: download URLs + Windows Defender ----

function settingsModal() {
  return document.getElementById('settingsModal');
}

async function openSettings() {
  const modal = settingsModal();
  if (!modal) return;
  modal.style.display = 'flex';
  await Promise.all([loadSettingsUrls(), refreshDefenderStatus()]);
}

function closeSettings() {
  const modal = settingsModal();
  if (modal) modal.style.display = 'none';
}

async function loadSettingsUrls() {
  const fields = {
    urlReshadeSetup: 'reshade_setup_url',
    urlReshade: 'reshade_url',
    urlOptiscaler: 'optiscaler_url',
    urlDlss5: 'dlss5_url',
    urlDgvoodoo: 'dgvoodoo_url',
    urlFeeder: 'feeder_url'
  };
  try {
    const urls = await GetDownloadURLs();
    for (const [id, key] of Object.entries(fields)) {
      const el = document.getElementById(id);
      if (el) el.value = (urls && urls[key]) || '';
    }
    const apiEl = document.getElementById('dgvoodooApi');
    if (apiEl) apiEl.value = (urls && urls.dgvoodoo_api === 'd3d12') ? 'd3d12' : 'd3d11';
    const consumerEl = document.getElementById('neuralConsumer');
    if (consumerEl) consumerEl.value = (urls && urls.neural_consumer === 'dfc') ? 'dfc' : 'renodx';
  } catch (err) {
    console.error('Failed to load download URLs:', err);
  }
}

async function handleSaveUrls() {
  const btn = document.getElementById('saveUrlsBtn');
  const result = document.getElementById('urlsSaveResult');
  const urls = {
    reshade_setup_url: document.getElementById('urlReshadeSetup')?.value.trim() || '',
    reshade_url: document.getElementById('urlReshade')?.value.trim() || '',
    optiscaler_url: document.getElementById('urlOptiscaler')?.value.trim() || '',
    dlss5_url: document.getElementById('urlDlss5')?.value.trim() || '',
    dgvoodoo_url: document.getElementById('urlDgvoodoo')?.value.trim() || '',
    dgvoodoo_api: document.getElementById('dgvoodooApi')?.value || 'd3d11',
    neural_consumer: document.getElementById('neuralConsumer')?.value || 'renodx',
    feeder_url: document.getElementById('urlFeeder')?.value.trim() || ''
  };
  if (btn) btn.disabled = true;
  if (result) { result.className = 'settings-result'; result.textContent = 'Saving...'; }
  try {
    const res = await SaveDownloadURLs(urls);
    if (result) {
      const ok = res && (res.success || res.Success);
      result.className = 'settings-result ' + (ok ? 'status-green-text' : 'status-red-text');
      result.textContent = ok ? 'Saved.' : (res.message || res.Message || 'Save failed');
    }
  } catch (err) {
    if (result) { result.className = 'settings-result status-red-text'; result.textContent = 'Save failed: ' + err; }
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function refreshDefenderStatus() {
  const statusEl = document.getElementById('defenderStatusText');
  const dirEl = document.getElementById('defenderAppDir');
  const resEl = document.getElementById('defenderResult');
  if (resEl) { resEl.className = 'settings-result'; resEl.textContent = ''; }
  try {
    const st = await GetDefenderStatus();
    if (!st) return null;
    if (dirEl) dirEl.textContent = st.appDir || st.AppDir || '';
    if (statusEl) {
      const excluded = !!(st.excluded ?? st.Excluded);
      const available = !!(st.available ?? st.Available);
      const err = st.error || st.Error || '';
      if (!available) {
        statusEl.className = 'settings-status status-muted-text';
        statusEl.textContent = '⚠ Defender status unreadable' + (err ? ' — ' + err : '');
      } else if (excluded) {
        statusEl.className = 'settings-status status-green-text';
        statusEl.textContent = '✓ App folder is excluded from Defender';
      } else {
        statusEl.className = 'settings-status status-red-text';
        statusEl.textContent = '✕ App folder is NOT excluded from Defender';
      }
    }
    return st;
  } catch (err) {
    console.error('Failed to get Defender status:', err);
    if (statusEl) { statusEl.className = 'settings-status status-muted-text'; statusEl.textContent = '⚠ Could not check Defender status'; }
    return null;
  }
}

async function handleExcludeApp() {
  const btn = document.getElementById('excludeAppBtn');
  const resEl = document.getElementById('defenderResult');
  if (btn) { btn.disabled = true; btn.textContent = 'Requesting...'; }
  if (resEl) { resEl.className = 'settings-result'; resEl.textContent = 'Waiting for the Windows permission prompt...'; }
  try {
    const st = await GetDefenderStatus();
    const appDir = st && (st.appDir || st.AppDir);
    if (!appDir) throw 'Could not resolve app folder';
    const res = await AddDefenderExclusion(appDir);
    const ok = res && (res.success || res.Success);
    if (resEl) {
      resEl.className = 'settings-result ' + (ok ? 'status-green-text' : 'status-red-text');
      resEl.textContent = res.message || res.Message || (ok ? 'Excluded.' : 'Failed');
    }
    await refreshDefenderStatus();
  } catch (err) {
    if (resEl) { resEl.className = 'settings-result status-red-text'; resEl.textContent = 'Failed: ' + err; }
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Exclude App Folder'; }
  }
}

// updateExcludeBtnState colors the header shield by exclusion state:
// filled green = folder excluded, hollow gray = not excluded.
async function updateExcludeBtnState() {
  const btn = document.getElementById('excludeGameBtn');
  if (!btn || !selectedGame || !selectedGame.path) return;
  btn.textContent = '🛡';
  btn.classList.remove('btn-excluded', 'btn-not-excluded');
  try {
    const excluded = await IsPathExcluded(selectedGame.path);
    // The game may have been switched while awaiting; only style the current one.
    if (!selectedGame || document.getElementById('excludeGameBtn') !== btn) return;
    btn.classList.add(excluded ? 'btn-excluded' : 'btn-not-excluded');
    btn.title = excluded
      ? 'Game folder IS excluded from Defender — click to remove the exclusion'
      : 'Game folder is NOT excluded from Defender — click to exclude it (wrapper/hook files may be quarantined)';
  } catch (err) {
    console.error('Failed to check exclusion state:', err);
  }
}

async function handleOpenFolder() {
  if (!selectedGame || !selectedGame.path) return;
  try {
    await OpenGameFolder(selectedGame.path);
  } catch (err) {
    showResult('error', 'Failed to open folder: ' + err);
  }
}

async function handleExcludeGame() {
  if (!selectedGame || !selectedGame.path) {
    showResult('error', 'Please select a game first');
    return;
  }
  const btn = document.getElementById('excludeGameBtn');
  const patchResult = document.getElementById('patchResult');
  if (btn) { btn.disabled = true; btn.textContent = '⏳'; }
  if (patchResult) { patchResult.className = 'result'; patchResult.textContent = 'Waiting for the Windows permission prompt...'; }
  try {
    const excluded = await IsPathExcluded(selectedGame.path);
    const res = excluded
      ? await RemoveDefenderExclusion(selectedGame.path)
      : await AddDefenderExclusion(selectedGame.path);
    if (res && (res.success || res.Success)) {
      showResult('success', res.message || res.Message);
    } else {
      showResult('error', (res && (res.message || res.Message)) || 'Failed');
    }
  } catch (err) {
    showResult('error', 'Failed: ' + err);
  } finally {
    if (btn) btn.disabled = false;
    updateExcludeBtnState();
  }
}

// On startup, if Defender is reachable but the app folder is not excluded,
// offer a one-click exclusion dialog. Shown every launch until excluded.
async function checkDefenderOnStartup() {
  try {
    const st = await GetDefenderStatus();
    if (!st) return;
    const excluded = !!(st.excluded ?? st.Excluded);
    const available = !!(st.available ?? st.Available);
    if (excluded || !available) return;
    const appDir = st.appDir || st.AppDir || '';
    const ok = await showConfirmModal({
      title: '🛡 Windows Defender Exclusion',
      bodyHtml: `
        <p>The app folder is <strong>not excluded</strong> from Windows Defender. Files like dgVoodoo2 are often false-flagged and silently quarantined, which breaks patching.</p>
        <p>Exclude it now in <strong>one click</strong>? (A Windows permission prompt will appear — that is normal.)</p>
        <p class="settings-path">${escapeHtml(appDir)}</p>
      `,
      confirmText: 'Exclude Now',
      cancelText: 'Later'
    });
    if (!ok) return;
    try {
      const res = await AddDefenderExclusion(appDir);
      if (res && (res.success || res.Success)) {
        openSettings();
      } else {
        await showConfirmModal({
          title: 'Defender Exclusion Failed',
          bodyHtml: `<p>${escapeHtml(res.message || res.Message || 'Unknown error')}</p><p>You can retry anytime from ⚙ Settings.</p>`,
          confirmText: 'OK',
          cancelText: 'Open Settings'
        }).then((ack) => { if (!ack) openSettings(); });
      }
    } catch (err) {
      console.error('Startup exclusion failed:', err);
    }
  } catch (err) {
    console.error('Startup Defender check failed:', err);
  }
}

// Event listeners
async function handleRefresh() {
  const refreshBtn = document.getElementById('refreshBtn');
  if (refreshBtn) {
    refreshBtn.disabled = true;
    refreshBtn.classList.add('spinning');
  }
  await loadGames();
  if (refreshBtn) {
    refreshBtn.disabled = false;
    refreshBtn.classList.remove('spinning');
  }
}

document.getElementById('settingsBtn')?.addEventListener('click', openSettings);
document.getElementById('settingsCloseBtn')?.addEventListener('click', closeSettings);
document.getElementById('saveUrlsBtn')?.addEventListener('click', handleSaveUrls);
document.getElementById('excludeAppBtn')?.addEventListener('click', handleExcludeApp);
settingsModal()?.addEventListener('click', (e) => {
  if (e.target && e.target.id === 'settingsModal') closeSettings();
});

document.getElementById('refreshBtn')?.addEventListener('click', handleRefresh);
document.getElementById('browseBtn').addEventListener('click', handleBrowseExe);
document.getElementById('patchBtn').addEventListener('click', handlePatch);
document.getElementById('uninstallBtn').addEventListener('click', handleUninstall);
document.getElementById('verifyBtn')?.addEventListener('click', handleVerify);
document.getElementById('launchBtn').addEventListener('click', handleLaunch);

// Mode toggle handlers
document.getElementById('modeReshade')?.addEventListener('click', () => {
  modeManuallySet = true;
  setPatchMode('reshade');
});
document.getElementById('modeOptiscaler')?.addEventListener('click', () => {
  modeManuallySet = true;
  setPatchMode('optiscaler');
});

function setPatchMode(mode) {
  patchMode = mode;
  document.getElementById('modeReshade').classList.toggle('mode-btn-active', mode === 'reshade');
  document.getElementById('modeOptiscaler').classList.toggle('mode-btn-active', mode === 'optiscaler');
  updatePatchButton();
}

function updatePatchButton() {
  const patchBtn = document.getElementById('patchBtn');
  const uninstallBtn = document.getElementById('uninstallBtn');
  if (patchMode === 'optiscaler') {
    patchBtn.textContent = selectedGame?.isInstalled
      ? 'Re-Patch (OptiScaler)'
      : 'Patch (OptiScaler)';
  } else {
    patchBtn.textContent = selectedGame?.isInstalled
      ? 'Re-Patch (ReShade)'
      : 'Patch (ReShade)';
  }
  patchBtn.title = patchMode === 'optiscaler'
    ? 'Install OptiScaler (FSR/XeSS/DLSS)'
    : 'Install ReShade + DLSS 5 Add-On';
  uninstallBtn.textContent = 'Uninstall';
  uninstallBtn.title = 'Uninstall Patch';
}
document.getElementById('discordBtn').addEventListener('click', (e) => {
  e.preventDefault();
  BrowserOpenURL('https://discord.gg/DZRMwdnYs');
});

document.addEventListener('click', (e) => {
  const link = e.target.closest('a[href]');
  if (link) {
    const href = link.getAttribute('href');
    if (href && (href.startsWith('http://') || href.startsWith('https://'))) {
      e.preventDefault();
      BrowserOpenURL(href);
    }
  }
});

// Initialize
loadGames();
loadVersion();
loadGpuInfo();
setupGpuControls();
checkDefenderOnStartup();