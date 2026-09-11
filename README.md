# xunlei-api

为已有的迅雷 Docker 提供独立 HTTP API：投递下载、查询状态、暂停、恢复、失败重试和删除。使用 Go 标准库实现，运行时只有静态二进制和 CA 证书，镜像基于 `scratch`，以非 root 用户运行，支持 `linux/amd64`、`linux/arm64`。

```text
Chrome 插件 / OpenClaw / 其他服务
             ↓ Bearer API Key
         xunlei-api
             ↓ 迅雷网页接口
      已登录的迅雷 Docker
```

本项目不包含迅雷下载引擎，不代办迅雷账号登录。请先准备可以正常下载的迅雷 Docker。适配依据为迅雷引擎 **3.21.0**；包装器镜像版本与引擎版本可能不同。内部接口可能随迅雷更新而变化，其他版本须自行验证。项目与迅雷无隶属关系。

## 功能

- HTTP / HTTPS / magnet 资源解析和任务投递；原始 `.torrent` 上传及文件索引选择。
- 设备、目录、任务列表和任务详情；返回规范化状态、进度、速度和观察时间。
- 创建目录、暂停、恢复、失败任务重试；默认删除记录并保留下载文件。
- 单个 API Key 认证；可配置准确的浏览器来源，供 Chrome 插件调用。
- 持久化幂等记录和操作状态；区分请求已接受、状态已确认以及结果未知。
- GitHub Actions 测试后发布 GHCR 多架构镜像，并生成 Linux 二进制、校验和、镜像 SBOM 与构建来源证明。

已在迅雷引擎 3.21.0 验证 HTTP / HTTPS 下载完成、暂停恢复、幂等投递、两种删除及 BT 选文件投递；BT 完整下载和真实失败重试尚未完成实测。详细记录见 [兼容性与验证](docs/compatibility.md)。

## 快速开始

下载 [compose.yaml](compose.yaml) 和 [.env.example](.env.example)，将 `.env.example` 复制为 `.env`。生成随机密钥并写入 `.env`：

```sh
openssl rand -hex 32
```

配置 `API_KEY` 和 `XUNLEI_BASE_URL`。后者是**API 容器可以访问到的迅雷地址**，例如 `http://xunlei:2345`，不需要拼接 `index.cgi`。`xunlei` 这个名称只有在两个容器处于同一 Docker 网络时才能解析；如果迅雷已在其他网络运行，应接入该网络，或填写容器可访问的主机地址。可以把 `DEFAULT_DESTINATION_PATH` 设置为迅雷界面中的常用目录，例如 `/迅雷下载`。

```sh
docker compose up -d
```

示例将服务绑定到宿主机 `127.0.0.1:8080`。其他机器或浏览器插件通过 HTTPS 反向代理访问；按实际部署调整网络配置。默认命名卷保存操作和幂等记录，删除卷会丢失这些记录。容器无需 Docker Socket，也无需挂载下载目录。

```sh
export XUNLEI_API_URL=http://127.0.0.1:8080
# 将下面的值换为自己的密钥；勿提交到源码仓库。
export XUNLEI_API_KEY='your-generated-api-key'

curl -fsS "$XUNLEI_API_URL/healthz"
curl -fsS -H "Authorization: Bearer $XUNLEI_API_KEY" \
  "$XUNLEI_API_URL/v1/device"
curl -fsS -H "Authorization: Bearer $XUNLEI_API_KEY" \
  "$XUNLEI_API_URL/v1/directories"
```

目录响应包含可直接使用的 `display_path`。它由迅雷界面中的目录名称组成，例如 `/迅雷下载/电影`，不是 NAS 宿主机路径。创建任务时直接传这个路径，API 会自动转换成迅雷内部 ID：

```sh
curl -fsS -X POST "$XUNLEI_API_URL/v1/tasks" \
  -H "Authorization: Bearer $XUNLEI_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: download-example-001' \
  -d '{"url":"https://example.org/example.zip","destination_path":"/迅雷下载/电影"}'
```

配置了 `DEFAULT_DESTINATION_PATH` 后还可以省略 `destination_path`。原有 `destination_id` 仅为兼容旧调用方保留。

创建响应的 `operation.result` 包含任务结果。随后按任务 ID 查询；暂停、恢复、删除返回操作 ID，通过 `/v1/operations/{id}` 确认结果。每次新的操作使用新的 `Idempotency-Key`；同一操作因网络问题重发时复用原键和原始请求。

更多示例：[API 使用说明](docs/api.md)；机器可读文档：[OpenAPI](docs/openapi.yaml)，服务内的 `/openapi.yaml` 同样需要 API Key。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `XUNLEI_BASE_URL` | `http://xunlei:2345` | 现有迅雷服务的 HTTP / HTTPS 地址 |
| `API_KEY` / `API_KEY_FILE` | — | 必填且二选一，32–512 字符；文件方式适合 Docker secret |
| `DEFAULT_DESTINATION_PATH` | 空 | 默认迅雷界面目录，例如 `/迅雷下载`；配置后投递时可省略目录 |
| `LISTEN_ADDR` | `:8080` | API 监听地址 |
| `DATA_DIR` | `/data` | 持久化操作和幂等记录目录；运行用户必须可写 |
| `XUNLEI_USERNAME` | 空 | 迅雷包装器的 HTTP Basic 用户名，可选 |
| `XUNLEI_PASSWORD` / `XUNLEI_PASSWORD_FILE` | 空 | HTTP Basic 密码；不同于迅雷账号密码 |
| `UPSTREAM_TIMEOUT` | `30s` | 单次上游 HTTP 请求超时 |
| `OPERATION_TIMEOUT` | `2m` | 控制请求的状态确认时限 |
| `CORS_ALLOWED_ORIGINS` | 空 | 逗号分隔的准确来源，如 `chrome-extension://YOUR_EXTENSION_ID`；不支持 `*` |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | 空 | 同时配置可直接启用 HTTPS；也可使用 HTTPS 反向代理 |

只有 `/healthz` 无需认证，并且仅用于进程存活检查；迅雷连接、登录情况请查询 `/v1/device`。所有客户端共享一个密钥，具有完整 API 操作权限。密钥只放在 `Authorization: Bearer …`，不放在 URL 或 Cookie 中。

文件密钥、HTTPS、Chrome 插件连接和存储权限示例见 [部署与认证](docs/deployment.md)。

## 删除与结果确认

`DELETE /v1/tasks/{id}` 默认只删除任务记录，保留文件。只有显式传入 `delete_files=true` 才会发送连文件删除指令。API 不挂载下载目录，因此任务记录消失并不能独立证明磁盘文件已被移除；请查看操作返回的状态和说明。

暂停、恢复等接口返回成功表示已提交请求，最终结果以 operation 和任务查询为准。下载进度达到 100% 时可能仍在校验，完成应以 `status=complete` 为准。失败重试保留已有文件、删除旧记录后重建，会产生新任务 ID。

上游没有原生幂等保证。API 会持久化请求并避免盲目重复投递，但上游收到请求与 API 保存响应之间崩溃时，仍可能出现 `unknown_outcome`。遇到该状态先核查现有任务，避免用新键重复投递。一个数据目录只运行一个服务实例。

## 开发与发布

本地需要 Go 1.25 或以上；CI 和发布镜像固定使用 Go 1.27.1。

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ./dist/xunlei-api ./cmd/xunlei-api
docker build -t xunlei-api:dev .
```

向仓库推送 `v0.1.0` 一类版本标签后，[发布流程](.github/workflows/release.yml) 先通过完整 CI，再使用 `GITHUB_TOKEN` 发布 `ghcr.io/myth815/xunlei-api:0.1.0`、`:0.1`、`:latest`。预发布标签不会覆盖稳定版的 `latest`。普通分支和外部 PR 只验证，不发布镜像。详细流程见 [发布说明](docs/releasing.md)。

## 致谢与许可证

遵循 [MIT License](LICENSE)。参考 [Kubespider 迅雷适配器](https://github.com/opennaslab/kubespider/tree/main/kubespider/download_provider/xunlei_download_provider) 的接入流程，以及 [cnk3x/xunlei](https://github.com/cnk3x/xunlei) 包装器所提供的现有网页接口；本项目独立实现 Go 客户端，不分发迅雷闭源组件。
