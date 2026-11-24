const state = {
  activeTab: "review",
  libraryStatus: "",
  toastTimer: null,
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(opts.headers || {}) },
    ...opts,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(data.error || `Request failed (${res.status})`);
  }
  return data;
}

function showToast(msg, type = "") {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.className = `toast ${type}`;
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => el.classList.add("hidden"), 4000);
}

function fmtDuration(sec) {
  if (!sec || sec <= 0) return "";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const pad = (n) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

function fmtDate(iso) {
  if (!iso) return "";
  try {
    return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
  } catch {
    return "";
  }
}

function fmtBytes(bytes) {
  if (!bytes || bytes <= 0) return "0 MB";
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(2)} GB`;
  const mb = bytes / (1024 * 1024);
  return `${mb.toFixed(1)} MB`;
}

function esc(s) {
  return String(s || "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

// --- Tabs ---
document.querySelectorAll(".tab").forEach((btn) => {
  btn.addEventListener("click", () => {
    state.activeTab = btn.dataset.tab;
    document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b === btn));
    document.querySelectorAll(".panel").forEach((p) => p.classList.toggle("active", p.id === `tab-${state.activeTab}`));
    refreshActiveTab();
  });
});

// --- Status ---
async function loadStatus() {
  try {
    const data = await api("/api/status");
    const st = data.stats;
    const bp = document.getElementById("badgePending");
    bp.textContent = st.pending_review;
    bp.classList.toggle("hidden", st.pending_review === 0);
    document.getElementById("badgeChannels").textContent = st.channels;
    document.getElementById("badgeDownloaded").textContent = st.downloaded;

    const lbl = document.getElementById("nextSyncLabel");
    if (data.sync_running) {
      lbl.textContent = "Syncing channels...";
    } else if (data.next_sync_at) {
      lbl.textContent = "Next sync: " + new Date(data.next_sync_at).toLocaleString();
    } else {
      lbl.textContent = "Next sync: —";
    }

    const storage = data.storage;
    const storageLbl = document.getElementById("storageLabel");
    const banner = document.getElementById("storageBanner");
    if (storage) {
      const limit = storage.hard_limit_bytes || storage.soft_limit_bytes;
      storageLbl.textContent = limit > 0
        ? `Storage: ${fmtBytes(storage.used_bytes)} / ${fmtBytes(limit)}`
        : `Storage: ${fmtBytes(storage.used_bytes)}`;
      storageLbl.className = "muted-pill";
      if (storage.hard_limit_reached) {
        storageLbl.classList.add("danger");
        banner.textContent = `Downloads paused: media storage (${fmtBytes(storage.used_bytes)}) reached the hard limit (${fmtBytes(storage.hard_limit_bytes)}). Delete videos or increase limits in Settings to resume.`;
        banner.classList.remove("hidden");
      } else {
        if (storage.soft_limit_bytes > 0 && storage.used_bytes >= storage.soft_limit_bytes) {
          storageLbl.classList.add("warn");
        }
        banner.classList.add("hidden");
      }
    }
  } catch (err) {
    console.error(err);
  }
}

// --- Review Queue ---
async function loadReviewQueue() {
  const videos = await api("/api/videos?status=pending_review&limit=200");
  const grid = document.getElementById("reviewGrid");
  const empty = document.getElementById("reviewEmpty");
  const approveAllBtn = document.getElementById("approveAllBtn");

  approveAllBtn.classList.toggle("hidden", videos.length === 0);
  empty.classList.toggle("hidden", videos.length > 0);
  grid.innerHTML = videos.map((v) => `
    <div class="video-card">
      <div class="thumb-wrap">
        <a href="https://www.youtube.com/watch?v=${esc(v.youtube_id)}" target="_blank" rel="noopener">
          <img src="${esc(v.thumbnail_url)}" alt="" loading="lazy" />
        </a>
        ${v.duration_seconds ? `<span class="duration-tag">${fmtDuration(v.duration_seconds)}</span>` : ""}
      </div>
      <div class="video-body">
        <a class="video-title" href="https://www.youtube.com/watch?v=${esc(v.youtube_id)}" target="_blank" rel="noopener">${esc(v.title)}</a>
        <div class="video-meta">${esc(v.channel_name)}${v.published_at ? " • " + fmtDate(v.published_at) : ""}</div>
      </div>
      <div class="video-actions">
        <button class="btn btn-success btn-sm" onclick="approveVideo(${v.id})">Approve</button>
        <button class="btn btn-danger btn-sm" onclick="rejectVideo(${v.id})">Reject</button>
      </div>
    </div>
  `).join("");
}

window.approveVideo = async (id) => {
  try {
    await api(`/api/videos/${id}/approve`, { method: "POST" });
    showToast("Approved and queued for download!", "success");
    await Promise.all([loadStatus(), refreshActiveTab()]);
  } catch (err) {
    showToast(err.message, "error");
  }
};

window.rejectVideo = async (id) => {
  try {
    await api(`/api/videos/${id}/reject`, { method: "POST" });
    showToast("Video rejected.");
    await Promise.all([loadStatus(), refreshActiveTab()]);
  } catch (err) {
    showToast(err.message, "error");
  }
};

window.deleteVideo = async (id) => {
  if (!confirm("Delete this video record and its downloaded file (if any)?")) return;
  try {
    await api(`/api/videos/${id}`, { method: "DELETE" });
    showToast("Video deleted.");
    await Promise.all([loadStatus(), refreshActiveTab()]);
  } catch (err) {
    showToast(err.message, "error");
  }
};

document.getElementById("approveAllBtn").addEventListener("click", async () => {
  if (!confirm("Approve all pending videos for download?")) return;
  try {
    const res = await api("/api/videos/approve-all", { method: "POST" });
    showToast(`Approved ${res.approved} video(s) for download!`, "success");
    await Promise.all([loadStatus(), loadReviewQueue()]);
  } catch (err) {
    showToast(err.message, "error");
  }
});

// --- Channels ---
async function loadChannels() {
  const channels = await api("/api/channels");
  const list = document.getElementById("channelsList");
  const empty = document.getElementById("channelsEmpty");
  empty.classList.toggle("hidden", channels.length > 0);
  list.innerHTML = channels.map((ch) => `
    <div class="channel-row">
      <div class="channel-info">
        ${ch.avatar_url ? `<img class="channel-avatar" src="${esc(ch.avatar_url)}" alt="" />` : `<div class="channel-avatar"></div>`}
        <div>
          <a class="channel-name" href="${esc(ch.url)}" target="_blank" rel="noopener">${esc(ch.name)}</a>
          <div class="video-meta">
            ${esc(ch.youtube_id)} • ${ch.downloaded_count || 0} downloaded • ${ch.pending_count || 0} pending
            ${ch.last_checked_at ? " • Checked " + fmtDate(ch.last_checked_at) : ""}
          </div>
        </div>
      </div>
      <div class="channel-controls">
        <label class="checkbox-label">
          <input type="checkbox" ${ch.auto_approve ? "checked" : ""} onchange="updateChannelFlag(${ch.id}, 'auto_approve', this.checked)" />
          Auto-approve
        </label>
        <label class="checkbox-label">
          <input type="checkbox" ${ch.skip_shorts ? "checked" : ""} onchange="updateChannelFlag(${ch.id}, 'skip_shorts', this.checked)" />
          Skip Shorts
        </label>
        <label class="checkbox-label">
          <input type="checkbox" ${ch.enabled ? "checked" : ""} onchange="updateChannelFlag(${ch.id}, 'enabled', this.checked)" />
          Enabled
        </label>
        <button class="btn btn-secondary btn-sm" onclick="syncChannel(${ch.id})">Sync</button>
        <button class="btn btn-danger btn-sm" onclick="deleteChannel(${ch.id})">Remove</button>
      </div>
    </div>
  `).join("");
}

document.getElementById("addChannelForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const btn = e.target.querySelector("button[type=submit]");
  const input = document.getElementById("channelInput").value.trim();
  const autoApprove = document.getElementById("channelAutoApprove").checked;
  const syncNow = document.getElementById("channelSyncNow").checked;
  btn.disabled = true;
  btn.textContent = "Resolving...";
  try {
    const res = await api("/api/channels", {
      method: "POST",
      body: JSON.stringify({ input, auto_approve: autoApprove, sync_now: syncNow }),
    });
    document.getElementById("channelInput").value = "";
    let msg = `Added channel "${res.channel.name}"`;
    if (res.sync) {
      msg += ` (${res.sync.new_pending} pending, ${res.sync.new_approved} auto-approved)`;
    }
    showToast(msg, "success");
    await Promise.all([loadStatus(), loadChannels()]);
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    btn.disabled = false;
    btn.textContent = "Add Channel";
  }
});

window.updateChannelFlag = async (id, field, value) => {
  try {
    await api(`/api/channels/${id}`, {
      method: "PUT",
      body: JSON.stringify({ [field]: value }),
    });
    showToast("Channel updated.", "success");
  } catch (err) {
    showToast(err.message, "error");
    loadChannels();
  }
};

window.syncChannel = async (id) => {
  showToast("Checking channel for new videos...");
  try {
    const res = await api(`/api/channels/${id}/sync`, { method: "POST" });
    showToast(`Sync done: ${res.new_pending} pending review, ${res.new_approved} auto-approved.`, "success");
    await Promise.all([loadStatus(), loadChannels()]);
  } catch (err) {
    showToast(err.message, "error");
  }
};

window.deleteChannel = async (id) => {
  if (!confirm("Remove this channel from monitoring? (Already downloaded videos will remain in your library.)")) return;
  try {
    await api(`/api/channels/${id}`, { method: "DELETE" });
    showToast("Channel removed.");
    await Promise.all([loadStatus(), loadChannels()]);
  } catch (err) {
    showToast(err.message, "error");
  }
};

// --- Library & Standalone Videos ---
document.querySelectorAll(".filter-btn").forEach((btn) => {
  btn.addEventListener("click", () => {
    state.libraryStatus = btn.dataset.status;
    document.querySelectorAll(".filter-btn").forEach((b) => b.classList.toggle("active", b === btn));
    loadLibrary();
  });
});

async function loadLibrary() {
  const qs = state.libraryStatus ? `?status=${encodeURIComponent(state.libraryStatus)}&limit=200` : "?limit=200";
  const videos = await api("/api/videos" + qs);
  const grid = document.getElementById("libraryGrid");
  const empty = document.getElementById("libraryEmpty");
  empty.classList.toggle("hidden", videos.length > 0);
  grid.innerHTML = videos.map((v) => `
    <div class="video-card">
      <div class="thumb-wrap">
        <a href="https://www.youtube.com/watch?v=${esc(v.youtube_id)}" target="_blank" rel="noopener">
          <img src="${esc(v.thumbnail_url)}" alt="" loading="lazy" />
        </a>
        <span class="status-pill status-${esc(v.status)}">${esc(v.status.replace("_", " "))}</span>
        ${v.duration_seconds ? `<span class="duration-tag">${fmtDuration(v.duration_seconds)}</span>` : ""}
      </div>
      <div class="video-body">
        <a class="video-title" href="https://www.youtube.com/watch?v=${esc(v.youtube_id)}" target="_blank" rel="noopener">${esc(v.title)}</a>
        <div class="video-meta">${esc(v.channel_name)}${v.published_at ? " • " + fmtDate(v.published_at) : ""}</div>
        ${v.error_message ? `<div class="video-error">${esc(v.error_message)}</div>` : ""}
      </div>
      <div class="video-actions">
        ${["failed", "rejected", "pruned", "pending_review"].includes(v.status)
          ? `<button class="btn btn-success btn-sm" onclick="approveVideo(${v.id})">${v.status === "failed" ? "Retry" : "Approve"}</button>`
          : ""}
        <button class="btn btn-danger btn-sm" onclick="deleteVideo(${v.id})">Delete</button>
      </div>
    </div>
  `).join("");
}

document.getElementById("addVideoForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const btn = e.target.querySelector("button[type=submit]");
  const input = document.getElementById("videoInput").value.trim();
  btn.disabled = true;
  btn.textContent = "Resolving...";
  try {
    const v = await api("/api/videos", {
      method: "POST",
      body: JSON.stringify({ input }),
    });
    document.getElementById("videoInput").value = "";
    showToast(`Queued "${v.title}" for download!`, "success");
    await Promise.all([loadStatus(), loadLibrary()]);
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    btn.disabled = false;
    btn.textContent = "Approve & Download";
  }
});

// --- Sync All ---
document.getElementById("syncNowBtn").addEventListener("click", async () => {
  const btn = document.getElementById("syncNowBtn");
  btn.disabled = true;
  btn.textContent = "Syncing...";
  try {
    const res = await api("/api/sync", { method: "POST" });
    showToast(`Checked ${res.channels_checked} channel(s): ${res.new_pending} pending review, ${res.new_approved} auto-approved.`, "success");
    await Promise.all([loadStatus(), refreshActiveTab()]);
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    btn.disabled = false;
    btn.textContent = "Sync Channels Now";
  }
});

// --- Settings ---
async function loadSettings() {
  const cfg = await api("/api/settings");
  const form = document.getElementById("settingsForm");
  for (const [k, v] of Object.entries(cfg)) {
    const el = form.elements[k];
    if (!el) continue;
    if (el.type === "checkbox") {
      el.checked = Boolean(v);
    } else {
      el.value = v ?? "";
    }
  }
}

function readSettingsForm() {
  const form = document.getElementById("settingsForm");
  const num = (name) => parseInt(form.elements[name].value, 10) || 0;
  const flt = (name) => parseFloat(form.elements[name].value) || 0;
  const str = (name) => form.elements[name].value.trim();
  return {
    sync_cron: str("sync_cron") || "0 0 * * *",
    check_limit_per_channel: num("check_limit_per_channel") || 15,
    max_resolution: num("max_resolution") || 1080,
    default_skip_shorts: form.elements["default_skip_shorts"].checked,
    default_skip_livestreams: form.elements["default_skip_livestreams"].checked,
    global_retention_days: num("global_retention_days"),
    global_retention_max_videos: num("global_retention_max_videos"),
    storage_soft_limit_gb: flt("storage_soft_limit_gb"),
    storage_hard_limit_gb: flt("storage_hard_limit_gb"),
    base_url: str("base_url"),
    plex_url: str("plex_url"),
    plex_token: str("plex_token"),
    plex_section_id: str("plex_section_id"),
    plex_labels: str("plex_labels"),
    notify_ntfy_url: str("notify_ntfy_url"),
    notify_ntfy_topic: str("notify_ntfy_topic"),
    notify_ntfy_token: str("notify_ntfy_token"),
    notify_discord_webhook: str("notify_discord_webhook"),
    notify_telegram_bot_token: str("notify_telegram_bot_token"),
    notify_telegram_chat_id: str("notify_telegram_chat_id"),
    notify_generic_webhook_url: str("notify_generic_webhook_url"),
  };
}

document.getElementById("settingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("/api/settings", {
      method: "PUT",
      body: JSON.stringify(readSettingsForm()),
    });
    showToast("Settings saved!", "success");
    loadStatus();
  } catch (err) {
    showToast(err.message, "error");
  }
});

document.getElementById("loadPlexSectionsBtn").addEventListener("click", async () => {
  const cfg = readSettingsForm();
  try {
    const sections = await api("/api/plex/sections", {
      method: "POST",
      body: JSON.stringify({ plex_url: cfg.plex_url, plex_token: cfg.plex_token }),
    });
    const list = document.getElementById("plexSectionsList");
    list.innerHTML = sections.map((s) =>
      `<button type="button" class="chip" onclick="selectPlexSection('${esc(s.key)}')">#${esc(s.key)} — ${esc(s.title)} (${esc(s.type)})</button>`
    ).join("");
    showToast(`Found ${sections.length} Plex library section(s). Click one to select it.`, "success");
  } catch (err) {
    showToast("Plex error: " + err.message, "error");
  }
});

window.selectPlexSection = (key) => {
  document.getElementById("settingsForm").elements["plex_section_id"].value = key;
  showToast(`Selected Plex section #${key}. Don't forget to Save Settings.`);
};

document.getElementById("refreshPlexBtn").addEventListener("click", async () => {
  try {
    await api("/api/plex/refresh", { method: "POST" });
    showToast("Triggered Plex library refresh!", "success");
  } catch (err) {
    showToast(err.message, "error");
  }
});

document.getElementById("applyPlexLabelsBtn").addEventListener("click", async () => {
  const cfg = readSettingsForm();
  try {
    const res = await api("/api/plex/apply-labels", {
      method: "POST",
      body: JSON.stringify({
        plex_url: cfg.plex_url,
        plex_token: cfg.plex_token,
        plex_section_id: cfg.plex_section_id,
        plex_labels: cfg.plex_labels,
      }),
    });
    showToast(`Applied label(s) [${res.labels.join(", ")}] to ${res.updated} video(s)!`, "success");
  } catch (err) {
    showToast("Plex label error: " + err.message, "error");
  }
});

document.getElementById("testNotifyBtn").addEventListener("click", async () => {
  try {
    const res = await api("/api/notifications/test", {
      method: "POST",
      body: JSON.stringify(readSettingsForm()),
    });
    showToast(`Sent test notification to ${res.sent} provider(s)!`, "success");
  } catch (err) {
    showToast("Notification test failed: " + err.message, "error");
  }
});

async function refreshActiveTab() {
  if (state.activeTab === "review") await loadReviewQueue();
  else if (state.activeTab === "channels") await loadChannels();
  else if (state.activeTab === "library") await loadLibrary();
  else if (state.activeTab === "settings") await loadSettings();
}

// Initial load & periodic status refresh
loadStatus();
refreshActiveTab();
setInterval(loadStatus, 15000);
