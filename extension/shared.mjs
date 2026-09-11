export const SETTINGS_KEY = "xunleiSettings";

export const DEFAULT_CATEGORIES = Object.freeze([
  { id: "default", name: "默认", path: "/downloads" },
  { id: "movie", name: "电影", path: "/downloads" },
  { id: "music", name: "音乐", path: "/downloads" },
  { id: "ebook", name: "电子书", path: "/downloads" },
]);

export function defaultSettings() {
  return {
    baseUrl: "",
    apiKey: "",
    categories: DEFAULT_CATEGORIES.map((category) => ({ ...category })),
  };
}

export function normalizeBaseUrl(value) {
  const input = String(value ?? "").trim();
  let parsed;
  try {
    parsed = new URL(input);
  } catch {
    throw new Error("远程地址必须是完整的 HTTP 或 HTTPS 地址");
  }
  if (!['http:', 'https:'].includes(parsed.protocol)) {
    throw new Error("远程地址只支持 HTTP 或 HTTPS");
  }
  if (parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new Error("远程地址不能包含账号、密码、查询参数或片段");
  }
  if (parsed.pathname !== "/" && parsed.pathname !== "") {
    throw new Error("远程地址只填写服务根地址，不要附加接口路径");
  }
  return parsed.origin;
}

export function originPattern(baseUrl) {
  return `${new URL(normalizeBaseUrl(baseUrl)).origin}/*`;
}

export function normalizeDestinationPath(value) {
  const input = String(value ?? "").trim();
  if (!input.startsWith("/")) {
    throw new Error("下载路径必须以 / 开头");
  }
  if (input.includes("\\") || input.includes("\0")) {
    throw new Error("下载路径包含不支持的字符");
  }
  const parts = input.split("/").filter(Boolean);
  if (parts.some((part) => part === "." || part === "..")) {
    throw new Error("下载路径不能包含 . 或 ..");
  }
  return parts.length ? `/${parts.join("/")}` : "/";
}

export function normalizeCategories(value) {
  const source = Array.isArray(value) ? value : [];
  const categories = [];
  const ids = new Set();
  const names = new Set();
  for (const item of source) {
    const id = String(item?.id ?? "").trim();
    const name = String(item?.name ?? "").trim();
    if (!id || !name || name.length > 40 || ids.has(id) || names.has(name)) {
      continue;
    }
    try {
      categories.push({ id, name, path: normalizeDestinationPath(item.path) });
      ids.add(id);
      names.add(name);
    } catch {
      // Invalid rows are ignored when repairing stored data.
    }
  }
  const defaultCategory = categories.find((category) => category.id === "default");
  if (!defaultCategory) {
    categories.unshift({ ...DEFAULT_CATEGORIES[0] });
  } else {
    defaultCategory.name = "默认";
    categories.splice(categories.indexOf(defaultCategory), 1);
    categories.unshift(defaultCategory);
  }
  return categories;
}

export function normalizeStoredSettings(value) {
  const defaults = defaultSettings();
  return {
    baseUrl: typeof value?.baseUrl === "string" ? value.baseUrl : defaults.baseUrl,
    apiKey: typeof value?.apiKey === "string" ? value.apiKey : defaults.apiKey,
    categories: normalizeCategories(value?.categories ?? defaults.categories),
  };
}

export function isSupportedDownloadUrl(value) {
  const input = String(value ?? "").trim();
  if (!input) return false;
  if (/^magnet:\?/i.test(input)) return true;
  try {
    return ["http:", "https:"].includes(new URL(input).protocol);
  } catch {
    return false;
  }
}

export function pickDownloadUrl(info = {}, tab = {}) {
  const candidates = [
    info.selectionText,
    info.linkUrl,
    info.srcUrl,
    info.pageUrl,
    tab.url,
  ];
  return candidates.map((value) => String(value ?? "").trim()).find(isSupportedDownloadUrl) ?? "";
}

function decodedBasename(url) {
  try {
    const parsed = new URL(url);
    const last = parsed.pathname.split("/").filter(Boolean).at(-1);
    return last ? decodeURIComponent(last) : parsed.hostname;
  } catch {
    return "";
  }
}

export function resourceLabel(url, tabTitle = "") {
  const input = String(url ?? "").trim();
  let label = "";
  if (/^magnet:\?/i.test(input)) {
    const match = input.match(/[?&]dn=([^&]+)/i);
    if (match) {
      try {
        label = decodeURIComponent(match[1].replace(/\+/g, " "));
      } catch {
        label = match[1];
      }
    }
    label ||= "磁力任务";
  } else {
    label = decodedBasename(input) || String(tabTitle ?? "").trim() || "下载任务";
  }
  return label.length > 72 ? `${label.slice(0, 69)}…` : label;
}

export function createTaskPayload(url, destinationPath) {
  if (!isSupportedDownloadUrl(url)) {
    throw new Error("当前内容不是可下载的 HTTP、HTTPS 或磁力地址");
  }
  return {
    url: String(url).trim(),
    destination_path: normalizeDestinationPath(destinationPath),
  };
}

export function apiErrorMessage(status, body) {
  const message = body?.error?.message ?? body?.operation?.message;
  if (typeof message === "string" && message.trim()) return message.trim();
  if (status === 401) return "认证信息不正确";
  if (status === 403) return "插件来源未被服务允许";
  if (status === 404) return "下载目录或接口不存在";
  if (status === 409) return "任务状态或幂等请求发生冲突";
  return `远程服务返回 HTTP ${status}`;
}
