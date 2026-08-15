# `/session` API

## go-judge 简介

`go-judge` 是一个快速、简单、安全的程序沙箱服务。它在受限制的运行环境中执行用户程序，并通过 HTTP REST API、WebSocket 和 gRPC 等方式提供调用接口。

服务默认监听 `http://localhost:5050`。常用接口包括：

- `POST /run`：执行一次性任务；
- `/file`：管理可供任务引用的文件；
- `/ws` 和 `/stream`：提供 WebSocket 及流式执行能力；
- `/session`：创建带持久化工作区的 Linux 有状态会话。

本文只描述 `/session`。会话功能仅在 Linux 环境提供，并且需要服务成功初始化 Linux workspace sandbox。若服务未启用会话能力，下面的路由不会注册。

如果服务使用 `-auth-token` 开启鉴权，请按服务的统一鉴权规则在每个请求中携带令牌。

## 会话模型

创建会话后，服务会为它分配一个不透明的 `sessionId`，并创建独立工作区。命令执行时，工作区映射为沙箱内的 `/w`，同一会话中的后续命令可以看到此前写入的文件和命令产生的文件。

会话具有以下约束：

- 默认空闲 TTL 为 30 分钟；TTL 到期且没有正在处理的请求时，会话及工作区会被自动删除。
- 默认工作区配额为 1024 MiB。文件写入和命令执行期间都会检查该配额。
- 会话命令使用独立的网络命名空间。
- 同一会话内的文件操作和命令执行会串行处理；不同会话受服务的全局并发配置限制。
- 工作区路径必须是相对路径，不能包含绝对路径或 `..`；符号链接不能绕过工作区边界。

所有成功的接口返回 HTTP `200 OK`。错误通常返回 JSON：

```json
{"error":"错误信息"}
```

常见错误状态码为：`400`（请求格式、参数或路径无效）、`404`（会话或文件不存在）、`408`（请求上下文超时或取消）、`413`（超出工作区配额）和 `500`（沙箱或其他内部错误）。

## API 定义

### 创建会话

```http
POST /session
Content-Type: application/json
```

请求体可省略，或传入：

| 字段 | 类型 | 单位 | 说明 |
| --- | --- | --- | --- |
| `ttl` | integer | 秒 | 空闲 TTL。`0` 使用服务默认值；必须为非负数。 |
| `maxDiskMB` | integer | MiB | 工作区配额。`0` 使用服务默认值；必须为非负数。 |

示例：

```bash
curl -X POST http://localhost:5050/session \
  -H 'Content-Type: application/json' \
  -d '{"ttl":1800,"maxDiskMB":256}'
```

响应：

```json
{
  "sessionId": "sess_0123456789abcdef",
  "createdAt": 1780000000
}
```

`createdAt` 是 Unix 时间戳（秒）。

### 写入工作区文件

```http
PUT /session/:id/file/*filepath
Content-Type: application/octet-stream
```

请求体会原样写入会话工作区中的 `filepath`。父目录会自动创建；同名文件会被替换。`filepath` 不能为空、不能是绝对路径，也不能包含 `..`。

```bash
curl -X PUT http://localhost:5050/session/sess_0123456789abcdef/file/src/main.py \
  -H 'Content-Type: application/octet-stream' \
  --data-binary $'print("hello")\n'
```

响应：

```json
{"status":"ok","path":"src/main.py","size":15}
```

`size` 是实际写入的字节数。

### 读取工作区文件

```http
GET /session/:id/file/*filepath
```

响应体为文件原始内容，`Content-Type` 为 `application/octet-stream`。

```bash
curl http://localhost:5050/session/sess_0123456789abcdef/file/src/main.py
```

文件不存在时返回 `404`。

### 列出工作区文件

```http
GET /session/:id/files
```

响应：

```json
{
  "files": [
    {"name":"src/main.py","size":15,"modTime":1780000000}
  ]
}
```

每个文件项包含：

| 字段 | 类型 | 单位 | 说明 |
| --- | --- | --- | --- |
| `name` | string | - | 相对于工作区的文件路径。 |
| `size` | integer | 字节 | 文件大小。 |
| `modTime` | integer | Unix 秒 | 文件最后修改时间。 |

只列出普通文件，不列出目录和符号链接。

### 执行命令

```http
POST /session/:id/exec
Content-Type: application/json
```

请求体：

| 字段 | 类型 | 单位 | 说明 |
| --- | --- | --- | --- |
| `args` | string[] | - | 必填。命令及其参数，`args[0]` 为可执行文件。 |
| `env` | string[] | - | 可选。环境变量，格式与 `/run` 相同，例如 `KEY=value`。 |
| `cpuLimit` | integer | 纳秒 | CPU 时间限制。 |
| `clockLimit` | integer | 纳秒 | 墙钟时间限制。 |
| `memoryLimit` | integer | 字节 | 内存限制。 |
| `procLimit` | integer | 个 | 进程数量限制。 |
| `stdin` | string | - | 可选，作为标准输入传给命令。 |

资源限制字段沿用 `/run` 的语义，时间值使用纳秒、内存值使用字节；零值不要当作“无限制”依赖，调用方应显式设置需要的限制。

示例：

```bash
curl -X POST http://localhost:5050/session/sess_0123456789abcdef/exec \
  -H 'Content-Type: application/json' \
  -d '{
    "args":["/usr/bin/python3","src/main.py"],
    "env":["LANG=C.UTF-8"],
    "cpuLimit":2000000000,
    "clockLimit":5000000000,
    "memoryLimit":268435456,
    "procLimit":32,
    "stdin":"input\n"
  }'
```

响应：

```json
{
  "status":"Accepted",
  "exitStatus":0,
  "time":1234567,
  "memory":1048576,
  "runTime":2345678,
  "stdout":"hello\n",
  "stderr":"",
  "error":""
}
```

| 字段 | 类型 | 单位 | 说明 |
| --- | --- | --- | --- |
| `status` | string | - | 执行状态，例如 `Accepted`、`Time Limit Exceeded`、`Memory Limit Exceeded`、`Nonzero Exit Status`、`Internal Error`。 |
| `exitStatus` | integer | - | 进程退出状态。 |
| `time` | integer | 纳秒 | CPU 时间。 |
| `memory` | integer | 字节 | 峰值内存。 |
| `runTime` | integer | 纳秒 | 实际运行时间。 |
| `stdout` | string | - | 标准输出。 |
| `stderr` | string | - | 标准错误。 |
| `error` | string | - | 运行器提供的附加错误信息，正常执行时通常为空。 |

标准输出和标准错误受服务端输出限制（默认 64 MiB，具体取决于服务配置）。命令执行成功返回 HTTP `200`，即使命令自身以非零状态退出；此时请检查响应中的 `status` 和 `exitStatus`。

### 下载工作区归档

```http
GET /session/:id/archive[?pattern=模式[,模式...]]
```

返回 `application/zip` 文件。省略 `pattern` 时归档工作区中的全部普通文件；指定后使用相对于工作区的 glob 模式筛选文件，例如 `*.py,src/*.json`。模式不能是绝对路径，也不能包含 `..`。

```bash
curl -o workspace.zip \
  'http://localhost:5050/session/sess_0123456789abcdef/archive?pattern=src/*.py'
```

### 销毁会话

```http
DELETE /session/:id
```

立即关闭会话、销毁沙箱环境并删除工作区。响应：

```json
{"status":"ok"}
```

会话销毁后，所有使用该 `sessionId` 的请求都返回 `404`。

## 最小调用流程

```text
POST /session
       |
       +--> PUT /session/:id/file/...  (准备源文件)
       +--> POST /session/:id/exec     (执行命令，可重复执行)
       +--> GET /session/:id/files     (查看工作区)
       +--> GET /session/:id/archive   (导出结果)
       +--> DELETE /session/:id        (释放资源)
```
