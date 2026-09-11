# API 使用说明

所有 `/v1/*` 请求使用同一个 `Authorization: Bearer <API_KEY>`。`GET /healthz` 只用于服务存活，不访问迅雷；`GET /v1/device` 返回真实设备连接信息。机器可读接口见 [openapi.yaml](openapi.yaml)。

## 接口

| 方法与路径 | 说明 | 幂等键 |
| --- | --- | --- |
| `GET /healthz` | 无需认证的存活检查 | — |
| `GET /openapi.yaml` | OpenAPI 文档 | — |
| `GET /v1/device` | 设备版本、在线、登录和磁盘信息 | — |
| `GET /v1/directories` | 目录分页，`parent_id`、`cursor`、`limit` | — |
| `POST /v1/directories` | `{ "parent_id": "…", "name": "…" }` | 必填 |
| `POST /v1/resources/resolve` | `{ "url": "…" }`，解析链接和文件树 | — |
| `POST /v1/resources/torrent` | multipart，字段名 `file`，上传原始种子并解析 | — |
| `GET /v1/tasks` | `status`、`cursor`、`limit` 分页 | — |
| `POST /v1/tasks` | 投递链接，明确指定目标目录 | 必填 |
| `GET /v1/tasks/{id}` | 指定任务详情 | — |
| `POST /v1/tasks/{id}/pause` | 暂停 | 必填 |
| `POST /v1/tasks/{id}/resume` | 开始／恢复；失败任务使用 retry | 必填 |
| `POST /v1/tasks/{id}/retry` | 保留文件，删除旧记录后重新创建 | 必填 |
| `DELETE /v1/tasks/{id}` | 默认保留文件；`delete_files=true` 连文件删除 | 必填 |
| `GET /v1/operations/{id}` | 查询操作状态 | — |

列表 `limit` 默认为 100，允许 1–200；任务筛选 `status` 支持 `all`、`active`、`pending`、`running`、`paused`、`complete`、`error`。`active` 包含等待、下载、暂停和失败任务；省略筛选等同 `all`。

返回列表的 `next_page_token` 非空时，将该值作为下一次请求的 `cursor`。不要把游标当成偏移量；查询任务时保持其他筛选参数一致。

## 投递与种子选择

创建普通链接或磁力任务：

```json
{
  "url": "https://example.org/example.zip",
  "destination_id": "DIRECTORY_ID",
  "name": "example.zip"
}
```

`name` 可省略，使用资源解析得到的名称。`destination_id` 必填，从目录接口取得。先解析资源，再用响应里的真实文件索引选择文件；索引 `0` 是有效值，不能用数组位置替代迅雷文件索引：

```json
{
  "url": "magnet:?xt=urn:btih:EXAMPLE_INFO_HASH",
  "destination_id": "DIRECTORY_ID",
  "file_indices": [0, 3, 7]
}
```

省略 `file_indices` 表示全部文件；空数组不是“全部”，会被拒绝。如果解析结果未完整列出文件，不能安全执行部分选择。

使用 API 返回的索引即可，无需自行补齐迅雷的原始字段。迅雷有时省略零索引，适配器仅在完整文件树中可以唯一确定时将其恢复为 `0`；有歧义时拒绝部分选择。真实 BT 测试已验证选择索引的投递与回读，完整下载的验证边界见 [兼容性记录](compatibility.md)。

上传种子并解析：

```sh
curl -fsS "$XUNLEI_API_URL/v1/resources/torrent" \
  -H "Authorization: Bearer $XUNLEI_API_KEY" \
  -F 'file=@example.torrent'
```

上传要求单个 `.torrent` 文件，大小为 1 字节至 16 MiB。上传与解析本身不创建下载任务。使用返回的 `url`，结合 `destination_id` 和需要的 `file_indices` 调用创建接口。原始种子会交给迅雷解析，不在 API 内改写为丢失 tracker 元数据的磁力链接。

## 状态和操作

任务状态规范化为 `pending`、`running`、`paused`、`complete`、`error`、`unknown`，同时保留上游 `phase`。`progress` 表示 0–100 百分比；上游没有数据时返回 `null`。`size_bytes` 和 `downloaded_bytes` 缺失也返回 `null`。`speed_bytes_per_second` 是字节／秒，`observed_at` 是读取上游的时间。

修改接口返回 `operation` 对象。创建目录、创建任务和重试新建任务成功为 HTTP 201；控制请求提交后为 HTTP 202；幂等重放或目标状态已经满足时为 HTTP 200。操作 `status`：

| 状态 | 含义 |
| --- | --- |
| `planned` | 请求已持久化，尚未确认发送结果 |
| `accepted` | 上游已接受，等待观察到目标状态 |
| `confirmed` | 已确认当前操作的结果；不代表整个下载任务完成 |
| `failed` | 已知失败，查看 `message` |
| `unknown_outcome` | 不能可靠判断是否生效，先核查任务，避免盲目重发 |

创建返回的 `operation.result` 包含创建的任务或目录。任务控制操作的 `task_id` 指向目标任务；重试通过 `previous_task_id` 关联原任务，成功后使用新的任务 ID。

```sh
curl -fsS -X POST "$XUNLEI_API_URL/v1/tasks/TASK_ID/pause" \
  -H "Authorization: Bearer $XUNLEI_API_KEY" \
  -H 'Idempotency-Key: pause-example-001'

curl -fsS "$XUNLEI_API_URL/v1/operations/OPERATION_ID" \
  -H "Authorization: Bearer $XUNLEI_API_KEY"
```

删除默认保留文件：

```sh
curl -fsS -X DELETE "$XUNLEI_API_URL/v1/tasks/TASK_ID" \
  -H "Authorization: Bearer $XUNLEI_API_KEY" \
  -H 'Idempotency-Key: remove-record-example-001'
```

显式连文件删除：

```sh
curl -fsS -X DELETE "$XUNLEI_API_URL/v1/tasks/TASK_ID?delete_files=true" \
  -H "Authorization: Bearer $XUNLEI_API_KEY" \
  -H 'Idempotency-Key: remove-files-example-001'
```

连文件删除只能请求迅雷执行，API 没有宿主机文件系统访问权；不能用“任务已查不到”独立证明磁盘文件已删除。连文件删除后即使记录消失，操作只记录 `result={"task_record_absent":true,"files_deleted":null}`，在确认超时后变为 `unknown_outcome`。只删记录成功则返回 `files_deleted=false`。

引擎 3.21.0 的专用测试已在 NAS 上独立确认这两种操作分别保留和删除了测试文件；这不会改变 API 的运行时确认范围。

重试会保留文件、删除旧任务记录再重建，中途失败时旧记录可能已被移除，调用方应读取操作结果。

## 幂等与恢复

对于表中要求幂等键的接口，同一次逻辑请求总是发送相同的 `Idempotency-Key` 和相同参数。键长度为 1–200 字符，不允许首尾空白。相同键与相同请求返回此前结果；相同键配不同请求返回冲突。每个新操作使用新键，包括对同一任务先暂停后恢复。

响应丢失或请求超时后，复用原键获取持久化结果。不要立刻换键再次创建任务。上游没有严格的原生幂等协议，网络不确定性或进程在上游已执行后崩溃时，操作会保守地报告结果未知。服务重启后必须保留原 `DATA_DIR`。

## 错误

请求校验等错误返回 `{ "error": { "code": "…", "message": "…" } }`。操作一旦建立，执行失败或状态冲突会返回 `{ "operation": { … } }`，保留可查询的操作 ID 和结果。

HTTP 400 表示请求校验问题，401 表示密钥缺失或错误，403 表示浏览器来源不被允许，404 表示查询对象不存在，409 表示幂等冲突或当前状态不支持此操作，415 表示 JSON 接口的 Content-Type 不正确。已经进入操作流程的后端错误返回 HTTP 502 和 operation；上游查询失败可能返回 502、503 或 504。调用方应检查 HTTP 状态，并同时检查 operation 状态，不能把“请求被接受”当成“已经完成”。
