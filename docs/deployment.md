# 部署与认证

## 网络与 HTTPS

API 必须能连接现有迅雷实例；迅雷负责下载和保存文件。将两个容器接入同一 Docker 网络后，可以使用 `http://xunlei:2345`。也可以填写 API 容器能够访问的主机地址。

仓库内 Compose 只在本机回环地址暴露 `8080`，适合在同一主机上用反向代理提供 HTTPS。跨机器调用应使用 HTTPS，以保护请求中的 API Key。代理应保留 `Authorization`、`Origin` 和 `Idempotency-Key` 请求头，并避免在访问日志记录认证头和下载链接。

如果直接由 API 终止 TLS，同时设置 `TLS_CERT_FILE`、`TLS_KEY_FILE` 并将证书文件只读挂载。密钥文件需要对 UID `65532` 可读。监听端口仍由 `LISTEN_ADDR` 决定。

## 单密钥认证

`API_KEY` 或 `API_KEY_FILE` 必填，密钥为 32–512 字符。建议 `openssl rand -hex 32` 生成 64 位十六进制值；所有调用方共享此密钥，具有完整权限。不要把服务密钥内置在公开发布的 Chrome 插件里；由使用者填写自己的服务地址和密钥。

只有 `/healthz` 无需认证。`/v1/*` 和 `/openapi.yaml` 都要求：

```http
Authorization: Bearer <API_KEY>
```

不支持 URL token 或 Cookie 登录。更换密钥后重启服务，并更新调用方。为浏览器配置的 CORS 只决定浏览器能否读取响应，并不能替代认证。

推荐用文件提供密钥，避免将实际值放进 Compose 文件：

```yaml
services:
  xunlei-api:
    image: ghcr.io/myth815/xunlei-api:latest
    environment:
      XUNLEI_BASE_URL: http://xunlei:2345
      API_KEY_FILE: /run/secrets/api_key
      DATA_DIR: /data
    secrets:
      - api_key
    volumes:
      - api-data:/data
    ports:
      - "127.0.0.1:8080:8080"
secrets:
  api_key:
    file: ./secrets/api_key
volumes:
  api-data:
```

文件挂载权限由部署方式决定，需确认非 root 的容器用户能够读取；不要同时设置 `API_KEY` 和 `API_KEY_FILE`。上游包装器如另有 HTTP Basic Auth，使用 `XUNLEI_USERNAME` 与 `XUNLEI_PASSWORD_FILE`，不要将这些凭据混同于 API Key 或迅雷账号登录。

## Chrome 插件

将扩展 ID 对应的完整来源加入配置，例如：

```text
CORS_ALLOWED_ORIGINS=chrome-extension://abcdefghijklmnopabcdefghijklmnop
```

也可以逗号分隔多个准确来源。不要加尾部 `/`，不接受通配符。默认不允许浏览器跨域访问；不带 `Origin` 的服务端客户端可以正常通过 Bearer Key 认证调用。浏览器预检允许 `Authorization`、`Content-Type`、`Idempotency-Key`；不使用 Cookie。

插件应让用户配置 API HTTPS 地址和密钥，使用浏览器存储保存个人设置，并按浏览器规则申请对应目标地址的 `host_permissions`。请求示例：

```js
const response = await fetch(`${apiBase}/v1/tasks`, {
  method: "POST",
  headers: {
    Authorization: `Bearer ${apiKey}`,
    "Content-Type": "application/json",
    "Idempotency-Key": crypto.randomUUID(),
  },
  body: JSON.stringify({ url, destination_id: destinationID }),
});
const result = await response.json();
```

为同一次提交生成一次幂等键，网络重试时复用；不要每次重试都调用 `crypto.randomUUID()`。

## 数据与运行权限

镜像以 UID/GID `65532:65532` 运行。命名卷初次创建时继承 `/data` 的目录所有权。使用宿主机目录挂载时，应事先创建专用目录，并将该目录所有者设为 `65532:65532`。操作和幂等记录可能包含资源链接和任务元数据，应该像服务配置一样保护。

一个数据目录只允许一个 API 实例使用。备份或迁移时先停止 API，再复制整个数据目录。删除该目录会失去幂等历史；同一个键在全新数据目录中会被视作新请求。

无需挂载下载目录，不需要特权模式、宿主机网络或 Docker Socket。示例开启只读根文件系统，并给 `/tmp` 配置容量有限的临时文件系统。容器健康检查由二进制自身完成，不依赖 shell 或 curl。
