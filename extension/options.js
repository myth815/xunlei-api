import {
  SETTINGS_KEY,
  apiErrorMessage,
  defaultSettings,
  normalizeBaseUrl,
  normalizeDestinationPath,
  normalizeStoredSettings,
  originPattern,
} from "./shared.mjs";

const elements = {
  baseUrl: document.querySelector("#base-url"),
  apiKey: document.querySelector("#api-key"),
  categories: document.querySelector("#categories"),
  connectionResult: document.querySelector("#connection-result"),
  pathResult: document.querySelector("#path-result"),
  save: document.querySelector("#save"),
  testConnection: document.querySelector("#test-connection"),
  verifyPaths: document.querySelector("#verify-paths"),
  addCategory: document.querySelector("#add-category"),
};

let categories = [];
let savedBaseUrl = "";

function setResult(element, type, message) {
  element.hidden = false;
  element.className = `result ${type}`;
  element.textContent = message;
}

function clearResult(element) {
  element.hidden = true;
  element.textContent = "";
  element.className = "result";
}

function renderCategories() {
  elements.categories.replaceChildren();
  categories.forEach((category, index) => {
    const row = document.createElement("div");
    row.className = "category-row";

    const name = document.createElement("input");
    name.type = "text";
    name.value = category.name;
    name.maxLength = 40;
    name.placeholder = "例如：电影";
    name.disabled = category.id === "default";
    name.setAttribute("aria-label", `第 ${index + 1} 个类目的名称`);
    name.addEventListener("input", () => { category.name = name.value; });

    const path = document.createElement("input");
    path.type = "text";
    path.value = category.path;
    path.placeholder = "/downloads";
    path.spellcheck = false;
    path.setAttribute("aria-label", `${category.name || `第 ${index + 1} 个类目`}的迅雷路径`);
    path.addEventListener("input", () => { category.path = path.value; });

    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "button danger";
    remove.textContent = category.id === "default" ? "默认路径" : "删除";
    remove.disabled = category.id === "default";
    remove.addEventListener("click", () => {
      categories = categories.filter((item) => item.id !== category.id);
      renderCategories();
      clearResult(elements.pathResult);
    });
    row.append(name, path, remove);
    elements.categories.appendChild(row);
  });
}

function collectSettings() {
  const baseUrl = normalizeBaseUrl(elements.baseUrl.value);
  const apiKey = elements.apiKey.value.trim();
  if (apiKey.length < 32 || apiKey.length > 512) {
    throw new Error("API Key 长度必须为 32–512 个字符");
  }

  const names = new Set();
  const normalizedCategories = categories.map((category) => {
    const name = String(category.name).trim();
    if (!name) throw new Error("类目名称不能为空");
    if (names.has(name)) throw new Error(`类目名称“${name}”重复`);
    names.add(name);
    return {
      id: category.id,
      name: category.id === "default" ? "默认" : name,
      path: normalizeDestinationPath(category.path),
    };
  });
  if (!normalizedCategories.some((category) => category.id === "default")) {
    throw new Error("必须保留默认下载路径");
  }
  return { baseUrl, apiKey, categories: normalizedCategories };
}

async function ensurePermission(baseUrl) {
  const origins = [originPattern(baseUrl)];
  // Keep the permission prompt directly inside the click handler's user gesture.
  const granted = await chrome.permissions.request({ origins });
  if (!granted) throw new Error("未授予访问远程地址的权限");
}

async function parseResponse(response) {
  const text = await response.text();
  try {
    return text ? JSON.parse(text) : {};
  } catch {
    return {};
  }
}

async function request(settings, path) {
  const response = await fetch(`${settings.baseUrl}${path}`, {
    headers: { Authorization: `Bearer ${settings.apiKey}` },
    signal: AbortSignal.timeout(30_000),
  });
  const body = await parseResponse(response);
  if (!response.ok) throw new Error(apiErrorMessage(response.status, body));
  return body;
}

async function withBusy(button, task) {
  button.disabled = true;
  try {
    await task();
  } catch (error) {
    throw error?.name === "TimeoutError" ? new Error("连接超时") : error;
  } finally {
    button.disabled = false;
  }
}

elements.save.addEventListener("click", () => {
  void withBusy(elements.save, async () => {
    clearResult(elements.connectionResult);
    try {
      const settings = collectSettings();
      await ensurePermission(settings.baseUrl);
      await chrome.storage.local.set({ [SETTINGS_KEY]: settings });
      const previousPattern = savedBaseUrl ? originPattern(savedBaseUrl) : "";
      const currentPattern = originPattern(settings.baseUrl);
      if (previousPattern && previousPattern !== currentPattern) {
        await chrome.permissions.remove({ origins: [previousPattern] });
      }
      categories = settings.categories.map((category) => ({ ...category }));
      savedBaseUrl = settings.baseUrl;
      elements.baseUrl.value = settings.baseUrl;
      renderCategories();
      setResult(elements.connectionResult, "success", "配置已保存，右键菜单已经更新。可以继续测试连接。");
    } catch (error) {
      setResult(elements.connectionResult, "error", error.message);
    }
  });
});

elements.testConnection.addEventListener("click", () => {
  void withBusy(elements.testConnection, async () => {
    clearResult(elements.connectionResult);
    try {
      const settings = collectSettings();
      await ensurePermission(settings.baseUrl);
      setResult(elements.connectionResult, "info", "正在连接远程迅雷…");
      const device = await request(settings, "/v1/device");
      const connection = device.online ? "在线" : "离线";
      const login = device.logged_in ? "已登录" : "未登录";
      const version = device.version ? `，引擎 ${device.version}` : "";
      const type = device.online && device.logged_in ? "success" : "error";
      setResult(elements.connectionResult, type, `连接成功：迅雷${connection}、${login}${version}`);
    } catch (error) {
      setResult(elements.connectionResult, "error", `连接失败：${error.message}`);
    }
  });
});

elements.verifyPaths.addEventListener("click", () => {
  void withBusy(elements.verifyPaths, async () => {
    clearResult(elements.pathResult);
    try {
      const settings = collectSettings();
      await ensurePermission(settings.baseUrl);
      setResult(elements.pathResult, "info", "正在验证下载路径…");
      const failures = [];
      for (const category of settings.categories) {
        try {
          await request(settings, `/v1/directories?path=${encodeURIComponent(category.path)}`);
        } catch (error) {
          failures.push(`${category.name}（${category.path}）：${error.message}`);
        }
      }
      if (failures.length) {
        setResult(elements.pathResult, "error", `以下路径不可用：${failures.join("；")}`);
      } else {
        setResult(elements.pathResult, "success", `已验证 ${settings.categories.length} 个下载路径，全部可用。`);
      }
    } catch (error) {
      setResult(elements.pathResult, "error", error.message);
    }
  });
});

elements.addCategory.addEventListener("click", () => {
  categories.push({ id: crypto.randomUUID(), name: "新类目", path: "/downloads" });
  renderCategories();
  clearResult(elements.pathResult);
  elements.categories.lastElementChild?.querySelector("input:not(:disabled)")?.focus();
});

async function initialize() {
  document.querySelector("#extension-id").textContent = chrome.runtime.id;
  const stored = await chrome.storage.local.get(SETTINGS_KEY);
  const settings = normalizeStoredSettings(stored[SETTINGS_KEY] ?? defaultSettings());
  savedBaseUrl = settings.baseUrl;
  elements.baseUrl.value = settings.baseUrl;
  elements.apiKey.value = settings.apiKey;
  categories = settings.categories.map((category) => ({ ...category }));
  renderCategories();
}

void initialize().catch((error) => setResult(elements.connectionResult, "error", error.message));
