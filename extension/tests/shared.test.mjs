import assert from "node:assert/strict";
import test from "node:test";

import {
  apiErrorMessage,
  createTaskPayload,
  normalizeBaseUrl,
  normalizeCategories,
  normalizeDestinationPath,
  originPattern,
  pickDownloadUrl,
  resourceLabel,
} from "../shared.mjs";

test("normalizes API origins and creates the least host permission", () => {
  assert.equal(normalizeBaseUrl(" https://xunlei-api.example.com/ "), "https://xunlei-api.example.com");
  assert.equal(originPattern("http://172.28.55.24:8080"), "http://172.28.55.24:8080/*");
  assert.throws(() => normalizeBaseUrl("https://user:secret@example.com"));
  assert.throws(() => normalizeBaseUrl("https://example.com/v1"));
  assert.throws(() => normalizeBaseUrl("ftp://example.com"));
});

test("keeps a default category and repairs invalid stored rows", () => {
  assert.deepEqual(normalizeCategories([])[0], { id: "default", name: "默认", path: "/downloads" });
  assert.deepEqual(
    normalizeCategories([
      { id: "movie", name: "电影", path: "/downloads//电影/" },
      { id: "movie-duplicate", name: "电影", path: "/ignored" },
      { id: "bad", name: "坏路径", path: "relative" },
    ]),
    [
      { id: "default", name: "默认", path: "/downloads" },
      { id: "movie", name: "电影", path: "/downloads/电影" },
    ],
  );
});

test("validates display paths", () => {
  assert.equal(normalizeDestinationPath("/downloads/电影/"), "/downloads/电影");
  assert.equal(normalizeDestinationPath("/"), "/");
  assert.throws(() => normalizeDestinationPath("downloads"));
  assert.throws(() => normalizeDestinationPath("/downloads/../secret"));
});

test("selects the intended downloadable resource", () => {
  assert.equal(
    pickDownloadUrl(
      { selectionText: "not a URL", linkUrl: "https://example.com/file.iso" },
      { url: "https://example.com/page" },
    ),
    "https://example.com/file.iso",
  );
  assert.equal(
    pickDownloadUrl({ selectionText: "magnet:?xt=urn:btih:abcdef" }, {}),
    "magnet:?xt=urn:btih:abcdef",
  );
  assert.equal(pickDownloadUrl({}, { url: "chrome://extensions" }), "");
});

test("builds path based API payloads and safe labels", () => {
  assert.deepEqual(createTaskPayload("https://example.com/a.zip", "/downloads"), {
    url: "https://example.com/a.zip",
    destination_path: "/downloads",
  });
  assert.equal(resourceLabel("https://example.com/releases/example%20file.zip"), "example file.zip");
  assert.equal(resourceLabel("magnet:?xt=urn:btih:abc&dn=Example+Movie"), "Example Movie");
  assert.throws(() => createTaskPayload("javascript:alert(1)", "/downloads"));
});

test("uses structured API errors", () => {
  assert.equal(apiErrorMessage(400, { error: { message: "路径无效" } }), "路径无效");
  assert.equal(apiErrorMessage(401, {}), "认证信息不正确");
  assert.equal(apiErrorMessage(503, {}), "远程服务返回 HTTP 503");
});
