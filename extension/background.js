import {
  SETTINGS_KEY,
  apiErrorMessage,
  createTaskPayload,
  defaultSettings,
  normalizeBaseUrl,
  normalizeStoredSettings,
  originPattern,
  pickDownloadUrl,
  resourceLabel,
} from "./shared.mjs";

const MENU_ROOT = "xunlei-add";
const MENU_CATEGORY_PREFIX = "xunlei-category:";
const MENU_SETTINGS = "xunlei-settings";
const MENU_CONTEXTS = ["page", "selection", "link", "image", "video", "audio"];
let menuUpdate = Promise.resolve();

async function readSettings() {
  const stored = await chrome.storage.local.get(SETTINGS_KEY);
  return normalizeStoredSettings(stored[SETTINGS_KEY]);
}

async function ensureSettings() {
  const stored = await chrome.storage.local.get(SETTINGS_KEY);
  const normalized = normalizeStoredSettings(stored[SETTINGS_KEY]);
  if (!stored[SETTINGS_KEY]) {
    await chrome.storage.local.set({ [SETTINGS_KEY]: defaultSettings() });
  } else if (JSON.stringify(normalized) !== JSON.stringify(stored[SETTINGS_KEY])) {
    await chrome.storage.local.set({ [SETTINGS_KEY]: normalized });
  }
  return normalized;
}

function createMenu(properties) {
  return new Promise((resolve, reject) => {
    chrome.contextMenus.create(properties, () => {
      const error = chrome.runtime.lastError;
      if (error) reject(new Error(error.message));
      else resolve();
    });
  });
}

async function rebuildMenusNow() {
  const settings = await ensureSettings();
  await chrome.contextMenus.removeAll();
  await createMenu({ id: MENU_ROOT, title: "添加到远程迅雷下载", contexts: MENU_CONTEXTS });
  for (const category of settings.categories) {
    await createMenu({
      id: `${MENU_CATEGORY_PREFIX}${category.id}`,
      parentId: MENU_ROOT,
      title: category.name,
      contexts: MENU_CONTEXTS,
    });
  }
  await createMenu({ parentId: MENU_ROOT, type: "separator", contexts: MENU_CONTEXTS });
  await createMenu({
    id: MENU_SETTINGS,
    parentId: MENU_ROOT,
    title: "配置下载类目…",
    contexts: MENU_CONTEXTS,
  });
}

function rebuildMenus() {
  menuUpdate = menuUpdate.catch(() => {}).then(rebuildMenusNow);
  return menuUpdate;
}

function renderPageToast(id, kind, title, content, persistent) {
  const rootId = "__remote_xunlei_toasts__";
  let root = document.getElementById(rootId);
  if (!root) {
    root = document.createElement("div");
    root.id = rootId;
    Object.assign(root.style, {
      position: "fixed",
      top: "20px",
      right: "20px",
      zIndex: "2147483647",
      display: "flex",
      flexDirection: "column",
      gap: "10px",
      width: "min(390px, calc(100vw - 40px))",
      pointerEvents: "none",
      fontFamily: "-apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif",
    });
    document.documentElement.appendChild(root);
  }

  let toast = document.getElementById(id);
  if (!toast) {
    toast = document.createElement("div");
    toast.id = id;
    toast.setAttribute("role", "status");
    Object.assign(toast.style, {
      boxSizing: "border-box",
      borderRadius: "12px",
      padding: "13px 14px",
      color: "#f8fafc",
      background: "#172033",
      boxShadow: "0 12px 30px rgba(15, 23, 42, .28)",
      border: "1px solid rgba(148, 163, 184, .28)",
      pointerEvents: "auto",
      opacity: "0",
      transform: "translateY(-8px)",
      transition: "opacity .16s ease, transform .16s ease",
    });
    root.appendChild(toast);
    requestAnimationFrame(() => {
      toast.style.opacity = "1";
      toast.style.transform = "translateY(0)";
    });
  }
  clearTimeout(toast.__remoteXunleiTimer);
  const color = kind === "success" ? "#22c55e" : kind === "error" ? "#f87171" : "#60a5fa";
  const icon = kind === "success" ? "✓" : kind === "error" ? "!" : "↓";
  toast.style.borderLeft = `4px solid ${color}`;
  toast.replaceChildren();

  const row = document.createElement("div");
  Object.assign(row.style, { display: "flex", gap: "10px", alignItems: "flex-start" });
  const badge = document.createElement("span");
  badge.textContent = icon;
  Object.assign(badge.style, {
    display: "grid",
    placeItems: "center",
    flex: "0 0 22px",
    height: "22px",
    borderRadius: "50%",
    color,
    background: "rgba(255,255,255,.08)",
    fontWeight: "800",
  });
  const copy = document.createElement("div");
  copy.style.minWidth = "0";
  const heading = document.createElement("div");
  heading.textContent = title;
  Object.assign(heading.style, { fontSize: "14px", fontWeight: "700", lineHeight: "20px" });
  const message = document.createElement("div");
  message.textContent = content;
  Object.assign(message.style, {
    marginTop: "3px",
    color: "#cbd5e1",
    fontSize: "13px",
    lineHeight: "19px",
    overflowWrap: "anywhere",
  });
  copy.append(heading, message);
  row.append(badge, copy);
  toast.append(row);

  if (!persistent) {
    toast.__remoteXunleiTimer = setTimeout(() => {
      toast.style.opacity = "0";
      toast.style.transform = "translateY(-8px)";
      setTimeout(() => toast.remove(), 180);
    }, kind === "error" ? 8000 : 4800);
  }
}

async function fallbackNotification(id, kind, message) {
  const options = {
    type: "basic",
    iconUrl: chrome.runtime.getURL("icons/icon128.png"),
    title: kind === "error" ? "远程迅雷下载失败" : "远程迅雷下载",
    message,
    priority: kind === "error" ? 2 : 0,
  };
  const updated = await chrome.notifications.update(id, options);
  if (!updated) await chrome.notifications.create(id, options);
}

async function notify(tabId, id, kind, message, persistent = false) {
  try {
    await chrome.scripting.executeScript({
      target: { tabId },
      func: renderPageToast,
      args: [id, kind, "远程迅雷下载", message, persistent],
    });
  } catch {
    await fallbackNotification(id, kind, message);
  }
}

function setResultBadge(tabId, success) {
  chrome.action.setBadgeBackgroundColor({ tabId, color: success ? "#15803d" : "#b91c1c" });
  chrome.action.setBadgeText({ tabId, text: success ? "✓" : "!" });
  setTimeout(() => chrome.action.setBadgeText({ tabId, text: "" }).catch(() => {}), 5000);
}

async function parseResponse(response) {
  const text = await response.text();
  try {
    return text ? JSON.parse(text) : {};
  } catch {
    return {};
  }
}

async function submitDownload(info, tab, categoryId) {
  const settings = await readSettings();
  const category = settings.categories.find((item) => item.id === categoryId);
  const tabId = tab?.id;
  if (!category || typeof tabId !== "number") return;

  const url = pickDownloadUrl(info, tab);
  const label = resourceLabel(url, tab?.title);
  const toastId = `remote-xunlei-${crypto.randomUUID()}`;

  if (!url) {
    await notify(tabId, toastId, "error", "没有找到可下载的 HTTP、HTTPS 或磁力地址");
    setResultBadge(tabId, false);
    return;
  }

  let baseUrl;
  try {
    baseUrl = normalizeBaseUrl(settings.baseUrl);
    if (settings.apiKey.length < 32) throw new Error("请先配置远程地址和认证信息");
    const allowed = await chrome.permissions.contains({ origins: [originPattern(baseUrl)] });
    if (!allowed) throw new Error("请在插件设置中重新保存远程地址并授予访问权限");
  } catch (error) {
    await notify(tabId, toastId, "error", error.message);
    setResultBadge(tabId, false);
    await chrome.runtime.openOptionsPage();
    return;
  }

  await notify(
    tabId,
    toastId,
    "info",
    `正在添加任务「${label}」到「${category.path}」下载目录…`,
    true,
  );

  try {
    const response = await fetch(`${baseUrl}/v1/tasks`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${settings.apiKey}`,
        "Content-Type": "application/json",
        "Idempotency-Key": `chrome-${crypto.randomUUID()}`,
      },
      body: JSON.stringify(createTaskPayload(url, category.path)),
      signal: AbortSignal.timeout(120_000),
    });
    const body = await parseResponse(response);
    if (!response.ok || ["failed", "unknown_outcome"].includes(body?.operation?.status)) {
      throw new Error(apiErrorMessage(response.status, body));
    }
    await notify(
      tabId,
      toastId,
      "success",
      `已成功添加任务「${label}」到「${category.path}」下载目录`,
    );
    setResultBadge(tabId, true);
  } catch (error) {
    const message = error?.name === "TimeoutError" ? "请求超时，请先查询任务列表再决定是否重试" : error.message;
    await notify(tabId, toastId, "error", `添加任务「${label}」失败：${message}`);
    setResultBadge(tabId, false);
  }
}

chrome.runtime.onInstalled.addListener((details) => {
  void ensureSettings()
    .then(rebuildMenus)
    .then(() => details.reason === "install" && chrome.runtime.openOptionsPage())
    .catch((error) => console.error("初始化远程迅雷插件失败", error));
});

chrome.runtime.onStartup.addListener(() => {
  void rebuildMenus().catch((error) => console.error("更新远程迅雷菜单失败", error));
});

chrome.storage.onChanged.addListener((changes, area) => {
  if (area === "local" && changes[SETTINGS_KEY]) {
    void rebuildMenus().catch((error) => console.error("更新远程迅雷菜单失败", error));
  }
});

chrome.action.onClicked.addListener(() => void chrome.runtime.openOptionsPage());

chrome.contextMenus.onClicked.addListener((info, tab) => {
  if (info.menuItemId === MENU_SETTINGS) {
    void chrome.runtime.openOptionsPage();
    return;
  }
  const menuId = String(info.menuItemId);
  if (menuId.startsWith(MENU_CATEGORY_PREFIX)) {
    void submitDownload(info, tab, menuId.slice(MENU_CATEGORY_PREFIX.length));
  }
});

void rebuildMenus().catch((error) => console.error("创建远程迅雷菜单失败", error));
