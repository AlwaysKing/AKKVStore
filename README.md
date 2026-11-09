# AKKV Store

AKKV Store 是一个自托管的 Key-Value 数据服务，使用 Go 和 SQLite 实现。它以单个二进制或单个容器运行，提供中文管理后台和面向程序的 HTTP API。

主要能力：

- 一个 project 包含多个 table。
- 每个 table 保存任意 UTF-8 key 和二进制 value。
- 每个 project 可创建多个独立访问 Token。
- 支持精确读写、条件写入、删除墓碑、时间过滤和分页查询。
- JSON、文本、图片和大型二进制文件都按原始字节存取。
- value 修改时间由客户端提交，服务端不自动维护业务修改时间。
- 不依赖外部数据库、Redis 或对象存储。

## 核心概念

```text
Project
└── Table
    └── Key -> Value
```

- **Project**：逻辑命名空间，需要在管理后台创建。
- **Table**：project 内的分类，名称在 project 内唯一。
- **Key**：table 内的唯一标识，可以包含 `/`。
- **Value**：任意二进制数据，最大大小由 `AKKV_MAX_VALUE_BYTES` 控制。
- **Project Token**：授权程序访问某个 project 的 API；同一个 project 可以有多个 Token。

Project 名称会保留创建时的大小写显示，但唯一性检查和 API 查询忽略大小写。例如 `MyApp`、`myapp` 和 `MYAPP` 是同一个 project，不能重复创建。

public API 只能访问已经存在且拥有有效 Token 的 project，不能在 API 中创建 project。

## Docker Compose 部署

### 1. 准备环境

需要：

- Docker Engine
- Docker Compose v2
- 可选：Caddy 或 Nginx，用于生产环境 HTTPS

复制配置文件：

```bash
cp .env.example .env
```

至少修改会话密钥：

```bash
openssl rand -base64 32
```

把生成结果写入 `.env`：

```dotenv
AKKV_SESSION_SECRET=生成的随机字符串
```

建议限制配置文件权限：

```bash
chmod 600 .env
```

### 2. 准备数据目录

容器以 UID/GID `10001` 运行：

```bash
mkdir -p data
sudo chown -R 10001:10001 data
chmod 700 data
```

### 3. 启动

```bash
docker compose up -d --build
docker compose logs -f
```

默认只绑定本机端口：

```text
http://127.0.0.1:8080
```

访问管理后台：

```text
http://127.0.0.1:8080/admin/
```

首次打开管理后台时，如果数据库尚未保存管理员密码，登录页会直接要求设置初始管理员密码。

> 初始密码设置完成前，不要把服务直接暴露到公网或不可信网络。

### 4. 健康检查

```bash
curl http://127.0.0.1:8080/healthz
```

正常返回：

```text
ok
```

容器内也配置了 Docker `HEALTHCHECK`。

### 5. 停止和升级

```bash
docker compose down
```

升级前先备份整个 `data/`，然后：

```bash
docker compose build --pull
docker compose up -d
```

## 首次使用

1. 打开 `/admin/` 并设置管理员初始密码。
2. 在项目首页创建 project。
3. 打开 project 设置，创建一个或多个访问 Token。
4. 在 Token 配置上方直接复制完整的 Project API 地址。
5. 为程序配置：

```bash
export KV_URL=https://kv.example.com
export KV_TOKEN=kv_xxxxxxxxxxxxxxxxxxxxxxxx
export KV_PROJECT=my-app
```

6. 使用公开 API 创建 table 或直接 PUT 第一个 key。

管理后台的“认证方式”页面支持：

- 密码认证
- Bark 推送认证
- 两种方式可以同时启用，但至少保留一种

Bark 地址必须是可接收推送的完整地址，例如：

```text
https://api.day.app/your-device-key
```

## 生产环境 HTTPS

生产环境应由 Caddy、Nginx 或其他反向代理提供 HTTPS，并保持：

```dotenv
AKKV_COOKIE_SECURE=true
```

如果只在本地通过 `http://127.0.0.1:8080` 调试，需要临时设置为：

```dotenv
AKKV_COOKIE_SECURE=false
```

### Caddy

```caddyfile
kv.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy 会自动处理证书和 `Host` 头。

### Nginx

```nginx
server {
    listen 443 ssl http2;
    server_name kv.example.com;

    ssl_certificate     /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    # 大 value 上传时不要被 Nginx 提前拒绝。
    client_max_body_size 0;

    location / {
        proxy_pass http://127.0.0.1:8080;

        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;

        # 大文件流式传输。
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

管理后台 POST 不校验 `Origin` / `Referer` 的 host，因此反向代理使用内网 Host 不会触发同源拒绝。管理后台访问权限仍由登录会话 Cookie 控制。

## 直接运行二进制

需要 Go 1.26 或更高版本：

```bash
go test ./...
go build -trimpath -o /tmp/akkvstore ./cmd/akkvstore
```

运行：

```bash
AKKV_SESSION_SECRET="$(openssl rand -base64 32)" \
AKKV_COOKIE_SECURE=false \
AKKV_DATA_DIR=/tmp/akkvstore-data \
/tmp/akkvstore
```

生产环境不要使用 `/tmp` 作为数据目录。

## 使用已发布镜像

镜像地址：

```text
ghcr.io/alwaysking/akkvstore
```

当前自动构建镜像仅发布 `linux/amd64`。

```bash
docker run -d \
  --name akkvstore \
  --restart unless-stopped \
  --env-file .env \
  -e AKKV_DATA_DIR=/data \
  -e AKKV_LISTEN=:8080 \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/data:/data" \
  --read-only \
  --tmpfs /tmp \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  ghcr.io/alwaysking/akkvstore:latest
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AKKV_LISTEN` | `:8080` | HTTP 监听地址 |
| `AKKV_DATA_DIR` | `./data` | 数据目录；Docker 镜像中默认为 `/data` |
| `AKKV_SYSTEM_DB_PATH` | `<data>/system.db` | 系统数据库路径 |
| `AKKV_SESSION_SECRET` | 启动时随机生成 | 管理后台 Cookie 签名密钥，至少 16 字符 |
| `AKKV_SESSION_TTL` | `12h` | 管理后台会话有效期，Go duration 格式 |
| `AKKV_COOKIE_SECURE` | `true` | Cookie 是否带 `Secure`；HTTPS 环境必须为 `true` |
| `AKKV_MAX_VALUE_BYTES` | `104857600` | 单个 value 最大字节数；`0` 表示不限制 |
| `AKKV_INLINE_THRESHOLD` | `262144` | 不超过该大小的 value 内联到 SQLite，超过则写入外部文件 |
| `AKKV_CORS_ORIGINS` | `*` | 允许的浏览器 Origin，逗号分隔；显式空值表示关闭 CORS |
| `TZ` | `Asia/Shanghai` | 管理后台显示时间使用的时区 |

说明：

- 未设置 `AKKV_SESSION_SECRET` 时，服务会生成临时密钥，重启后管理员需要重新登录。
- 不要把 `AKKV_MAX_VALUE_BYTES` 误解为 table 或 project 的总容量限制。
- `AKKV_INLINE_THRESHOLD` 不建议设置得过大，大 value 应使用外部文件路径。
- 设置 `AKKV_MAX_VALUE_BYTES=0` 时，反向代理的请求体限制也要同步放开。

## 数据目录和备份

典型目录结构：

```text
data/
├── system.db
├── system.db-wal
├── system.db-shm
├── projects/
│   └── <project-id>/
│       ├── project.db
│       ├── project.db-wal
│       ├── project.db-shm
│       └── blobs/
│           └── <table-id>/
└── tmp/
```

备份要求：

- `/data` 是唯一数据源。
- 推荐在服务停止时备份整个 `/data`。
- 使用文件系统、LVM、ZFS 或云盘快照时，必须保证所有 SQLite 文件和 `blobs/` 处于同一快照。
- 不要在服务运行时只复制 `system.db`，否则可能遗漏 WAL、project 数据库和外部 value 文件。
- 同一个 `/data` 只能由一个 AKKV Store 实例使用。

服务启动时会清理临时目录和已注册 project 之外的无主目录。不要手工把其他程序的数据放进该目录。

# 公开 API

## 基本信息

公开 API 只有以下入口：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/v1/time` | 获取服务器时间，无需 Token |
| `POST` | `/v1/api` | JSON-RPC 2.0 控制面 |
| `PUT` | `/v1/upload` | 原始二进制上传，写入 `file` 类型 |
| `GET` | `/v1/download` | 原始二进制下载 |

除 `/v1/time` 外，所有请求都需要 Project Token：

```http
Authorization: Bearer <project-token>
```

每个 Token 都拥有对应 project 内全部 table 和 key 的读写删除权限。Token 不应暴露给不受信任的浏览器端或客户端；生产环境必须使用 HTTPS。

### 时间

```http
GET /v1/time
```

无需 Token。

```json
{
  "serverTime": 1762600000123,
  "iso": "2026-11-09T12:00:00.123Z"
}
```

服务端只返回当前时间。客户端如何使用这个时间、如何同步以及如何保存最后同步位置，均由客户端决定。

## JSON-RPC

所有控制面操作统一使用：

```http
POST /v1/api
Authorization: Bearer <project-token>
Content-Type: application/json
```

请求必须是单个 JSON-RPC 2.0 对象：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "kv.get",
  "params": {
    "project": "my-app",
    "table": "users",
    "key": "user:1001"
  }
}
```

约定：

- `project` 必须放在 `params` 中。
- 不支持 JSON-RPC 通知和请求数组；业务批量使用 `batch.execute`。
- 成功返回 `result`，失败返回 `error`。
- `id` 会原样返回。
- 认证失败使用 HTTP `401`；其他 JSON-RPC 错误通常使用 HTTP `200` 和 `error` 对象。

成功响应：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "value": "hello"
  }
}
```

错误响应：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32002,
    "message": "key 不存在",
    "data": {
      "code": "not_found"
    }
  }
}
```

## 值类型

value 只有两种业务类型，使用整数 `contentType` 表示：

| 值 | 类型 | 写入入口 | 读取方式 |
| --- | --- | --- | --- |
| `0` | `string` | `kv.put` 或 `batch.execute` | JSON-RPC |
| `1` | `file` | `/v1/upload` | `/v1/download` |

类型由写入入口决定，客户端不能任意指定。服务端不保存 MIME 类型，文件内容如何解释由客户端自行决定。

JSON-RPC 返回文件记录时不会把文件内容塞进 JSON，只返回元信息和 `valueOmitted: true`。

## Table 方法

### `table.list`

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "table.list",
  "params": {
    "project": "my-app"
  }
}
```

响应：

```json
{
  "items": [
    {
      "name": "users",
      "keyCount": 12,
      "updatedAt": 1760000005000
    }
  ]
}
```

### `table.create`

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "table.create",
  "params": {
    "project": "my-app",
    "name": "users"
  }
}
```

table 名必须匹配 `[a-z0-9][a-z0-9_-]{0,31}`。

### `table.get` 和 `table.delete`

```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "method": "table.get",
  "params": {
    "project": "my-app",
    "table": "users"
  }
}
```

`table.delete` 使用相同参数，删除 table、全部 key 和对应外部文件，操作不可恢复。

## KV 方法

### `kv.put`

写入或覆盖 string 类型：

```json
{
  "jsonrpc": "2.0",
  "id": 5,
  "method": "kv.put",
  "params": {
    "project": "my-app",
    "table": "users",
    "key": "user:1001",
    "updatedAt": 1762490000123,
    "value": "{\"name\":\"张三\",\"vip\":true}"
  }
}
```

规则：

- `updatedAt` 必须是正整数 Unix 毫秒时间戳。
- `value` 必须是 UTF-8 字符串。
- 写入后的 `contentType` 固定为 `0`。
- table 不存在时，首次 `kv.put` 会自动创建 table。

### `kv.get`

```json
{
  "jsonrpc": "2.0",
  "id": 6,
  "method": "kv.get",
  "params": {
    "project": "my-app",
    "table": "users",
    "key": "user:1001"
  }
}
```

string 类型示例：

```json
{
  "key": "user:1001",
  "contentType": 0,
  "size": 25,
  "updatedAt": 1762490000123,
  "deleted": false,
  "value": "{\"name\":\"张三\",\"vip\":true}"
}
```

file 类型示例：

```json
{
  "key": "report.pdf",
  "contentType": 1,
  "size": 348212,
  "updatedAt": 1762490000124,
  "deleted": false,
  "valueOmitted": true
}
```

file 类型的实际内容使用 `/v1/download` 获取。

### `kv.delete`

```json
{
  "jsonrpc": "2.0",
  "id": 7,
  "method": "kv.delete",
  "params": {
    "project": "my-app",
    "table": "users",
    "key": "user:1001",
    "updatedAt": 1762490000125
  }
}
```

删除会写入墓碑，并立即清空 value 和对应外部文件。对不存在的 key 执行删除也会创建墓碑。

## 原子条件

条件写入和删除支持可选的 `condition` 参数：

```json
{
  "condition": {
    "all": [
      {
        "field": "updatedAt",
        "op": "lt",
        "value": 1762490000123
      },
      {
        "field": "contentType",
        "op": "eq",
        "value": 0
      }
    ]
  }
}
```

所有条件按 AND 计算，并与写操作在同一事务中执行。服务端不自动执行 Last-Write-Wins，只执行客户端给出的条件。

支持的字段：

| 字段 | 类型 | 操作符 |
| --- | --- | --- |
| `exists` | bool | `eq`、`ne` |
| `deleted` | bool | `eq`、`ne` |
| `updatedAt` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `size` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `contentType` | int（0/1） | `eq`、`ne` |
| `value` | string | `eq`、`ne`、`lt`、`lte`、`gt`、`gte`、`startsWith`、`endsWith` |

条件不成立时：

- JSON-RPC 返回 `error.code = -32005`，`data.current` 包含当前记录状态。
- `/v1/upload` 返回 HTTP `412`，响应体包含 `current`。

## 查询

### `records.query`

同时支持单 table 和整个 project 查询：

```json
{
  "jsonrpc": "2.0",
  "id": 8,
  "method": "records.query",
  "params": {
    "project": "my-app",
    "tables": ["users", "sessions"],
    "condition": {
      "all": [
        {
          "field": "updatedAt",
          "op": "gt",
          "value": 1762600000000
        }
      ]
    },
    "includeDeleted": false,
    "includeValues": false,
    "limit": 100,
    "offset": 0
  }
}
```

- 不传 `tables` 时查询整个 project。
- 单 table 查询只需要把 `tables` 设置为 `["users"]`。
- 排序固定为 table 名称、key 升序。
- `limit` 默认 100，最大 500。
- `includeValues=true` 时，只有 `contentType=0` 的记录返回 `value`。
- `contentType=1` 的记录只返回元信息和 `valueOmitted: true`。

响应：

```json
{
  "items": [
    {
      "table": "users",
      "key": "user:1001",
      "contentType": 0,
      "size": 25,
      "updatedAt": 1762600000000,
      "deleted": false,
      "value": "{\"name\":\"张三\"}"
    }
  ],
  "total": 1,
  "nextOffset": 1,
  "hasMore": false
}
```

查询字段：

| 字段 | 类型 | 操作符 |
| --- | --- | --- |
| `key` | string | `eq`、`ne`、`lt`、`lte`、`gt`、`gte`、`startsWith`、`endsWith`、`glob` |
| `updatedAt` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `deleted` | bool | `eq`、`ne` |
| `size` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `contentType` | int（0/1） | `eq`、`ne` |
| `value` | string | `eq`、`ne`、`lt`、`lte`、`gt`、`gte`、`startsWith`、`endsWith` |

服务端只执行请求中给出的条件和分页，不保证查询期间数据快照稳定，也不负责增量同步完整性。

## 批量操作

```json
{
  "jsonrpc": "2.0",
  "id": 9,
  "method": "batch.execute",
  "params": {
    "project": "my-app",
    "transaction": true,
    "operations": [
      {
        "op": "put",
        "table": "users",
        "key": "user:1001",
        "updatedAt": 1762600000000,
        "value": "{\"name\":\"Alice\"}"
      },
      {
        "op": "delete",
        "table": "sessions",
        "key": "session:old",
        "updatedAt": 1762600000001
      }
    ]
  }
}
```

规则：

- `transaction` 必须显式设置为 `true` 或 `false`。
- `true` 时全部成功或全部失败；`false` 时逐项执行。
- PUT 只能是 `contentType=0`，value 必须是字符串。
- DELETE 不需要 value。
- 同一批内不允许重复的 `(table,key)`。
- 最多 500 个 operation，请求体最大 32 MiB。
- 大文件不走 batch，使用多次 `/v1/upload`。

## 文件上传

```http
PUT /v1/upload
Authorization: Bearer <project-token>
AKKV-Index-Project: my-app
AKKV-Index-Table: files
AKKV-Index-Key: cmVwb3J0LnBkZg
AKKV-Data-Updated-At: 1762490000125
Content-Type: application/octet-stream

<原始文件字节>
```

Header 规则：

- `AKKV-Index-Project`：project 名称。
- `AKKV-Index-Table`：table 名称。
- `AKKV-Index-Key`：key 的 Base64URL 无填充编码。
- `AKKV-Data-Updated-At`：客户端最后修改时间，Unix 毫秒时间戳。
- `AKKV-Data-Condition`：可选，Base64URL 编码的 JSON 条件，最大解码后 8 KiB。
- `Content-Type` 只是 HTTP 传输类型，不保存为业务字段；建议使用 `application/octet-stream`。

成功返回 `204 No Content`。写入后的 `contentType` 固定为 `1`。

失败示例：

```json
{
  "error": {
    "code": "precondition_failed",
    "message": "key \"report.pdf\" precondition failed"
  },
  "current": {
    "key": "report.pdf",
    "exists": true,
    "deleted": false,
    "contentType": 0,
    "size": 25,
    "updatedAt": 1762490000123
  }
}
```

## 文件下载

```http
GET /v1/download
Authorization: Bearer <project-token>
AKKV-Index-Project: my-app
AKKV-Index-Table: files
AKKV-Index-Key: cmVwb3J0LnBkZg
```

成功返回原始字节：

```http
200 OK
Content-Type: application/octet-stream
Content-Length: 348212
ETag: "..."
Last-Modified: ...
```

没有单独的 `HEAD` 接口。需要文件元信息时先调用 `kv.get`，需要实际文件时调用 `GET /v1/download`。

`GET /v1/download` 仍支持标准 HTTP `Range` 和 `If-None-Match`。

## Key 规则

- 最长 1024 字节。
- 必须是合法 UTF-8。
- 不能包含控制字符。
- 可以包含 `/`，例如 `dir/sub/file`。
- 不能以 `/` 开头或结尾。
- 不能包含连续斜杠。
- 不能包含 `.` 或 `..` 路径段。
- HTTP Header 中的 key 使用 Base64URL 无填充编码。

生成 key Header：

```bash
printf %s 'dir/sub/file' |
  base64 |
  tr '+/' '-_' |
  tr -d '=\n'
```

## 错误和状态

JSON-RPC 错误使用整数 code：

| code | 含义 |
| --- | --- |
| `-32700` | JSON 解析错误 |
| `-32600` | JSON-RPC 请求格式错误 |
| `-32601` | 方法不存在 |
| `-32602` | 参数错误 |
| `-32603` | 服务内部错误 |
| `-32002` | key 不存在 |
| `-32003` | table 不存在 |
| `-32004` | 名称冲突 |
| `-32005` | 写入条件不成立 |
| `-32006` | 内容超过限制 |
| `-32007` | 查询参数错误 |
| `-32008` | 批量参数错误 |

HTTP 状态：

| 状态 | 说明 |
| --- | --- |
| `200` | JSON-RPC 成功或业务错误 |
| `204` | 上传成功 |
| `400` | JSON-RPC 请求体不是合法 JSON |
| `401` | Token 缺失、错误或 project 不匹配 |
| `404` | 上传/下载目标不存在 |
| `412` | 上传条件不成立 |
| `413` | 内容超过限制 |
| `416` | 下载 Range 不合法 |

project 不存在和 Token 不匹配都返回 `401`，不泄露 project 是否存在。

## CORS

默认：

```dotenv
AKKV_CORS_ORIGINS=*
```

限制为指定站点：

```dotenv
AKKV_CORS_ORIGINS=https://app.example.com,https://admin.example.com
```

关闭 CORS：

```dotenv
AKKV_CORS_ORIGINS=
```

预检允许的请求头：

```text
Authorization
Content-Type
AKKV-Index-Project
AKKV-Index-Table
AKKV-Index-Key
AKKV-Data-Updated-At
AKKV-Data-Condition
```

暴露的响应头：

```text
ETag
Content-Length
Last-Modified
```

## 完整 curl 示例

```bash
export KV_URL=http://127.0.0.1:8080
export KV_PROJECT=my-app
export KV_TOKEN=kv_xxxxxxxxxxxxxxxxxxxxxxxx
export MODIFIED_AT=1762490000123
```

创建 table：

```bash
curl -X POST "$KV_URL/v1/api" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary '{
    "jsonrpc":"2.0",
    "id":1,
    "method":"table.create",
    "params":{
      "project":"my-app",
      "name":"users"
    }
  }'
```

写入 string：

```bash
curl -X POST "$KV_URL/v1/api" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary '{
    "jsonrpc":"2.0",
    "id":2,
    "method":"kv.put",
    "params":{
      "project":"my-app",
      "table":"users",
      "key":"user:1001",
      "updatedAt":1762490000123,
      "value":"{\"name\":\"张三\",\"vip\":true}"
    }
  }'
```

读取 string：

```bash
curl -X POST "$KV_URL/v1/api" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary '{
    "jsonrpc":"2.0",
    "id":3,
    "method":"kv.get",
    "params":{
      "project":"my-app",
      "table":"users",
      "key":"user:1001"
    }
  }'
```

上传文件：

```bash
KEY="$(printf %s 'report.pdf' | base64 | tr '+/' '-_' | tr -d '=\n')"

curl -X PUT "$KV_URL/v1/upload" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "AKKV-Index-Project: $KV_PROJECT" \
  -H "AKKV-Index-Table: files" \
  -H "AKKV-Index-Key: $KEY" \
  -H "AKKV-Data-Updated-At: 1762490000125" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @report.pdf
```

下载文件：

```bash
curl "$KV_URL/v1/download" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "AKKV-Index-Project: $KV_PROJECT" \
  -H "AKKV-Index-Table: files" \
  -H "AKKV-Index-Key: $KEY" \
  -o report.pdf
```

## 设计边界

- 性能优先：SQLite 使用 WAL 和 `synchronous=OFF`，不等待每次提交刷盘；断电或系统崩溃时可能丢失最近写入，极端情况下可能损坏数据库文件。
- project.db 连接按 project 热持有，每个 project 最多 10 个连接；全局空闲缓存上限 32 个 project，连续 5 分钟无读写后自动关闭，活跃连接不会被强制驱逐。
- project 内普通读写直接交给 SQLite，由数据库文件锁和 `BEGIN IMMEDIATE` 串行化写事务，不使用全局业务写锁。
- project Token 鉴权结果最多缓存 30 秒；通过管理后台删除 Token 或 project 时立即失效。
- 访问统计先累计在内存中，约每秒批量写入 system.db；进程异常退出时允许丢失尚未落库的统计。
- `updatedAt` 由客户端维护，服务端不校验时钟准确性。
- 服务端不处理业务冲突，只执行客户端提供的原子条件。
- 不提供完整历史版本链。
- 不提供 TTL。
- 不提供原子数值递增。
- 批量写入支持一次事务或逐项独立执行，但不跨 project。
- 不提供服务端排序、聚合或业务字段搜索。
- 删除墓碑不会自动回收。
- 单管理员，不支持多用户和 RBAC。
- project 是逻辑隔离，不是对抗性多租户隔离。
- 所有 project 共享同一进程和同一块磁盘。
- 不自动备份，部署方负责备份和恢复演练。

## 本地开发

```bash
go test ./...
go test -race ./...
go vet ./...

go build -o /tmp/akkvstore ./cmd/akkvstore

AKKV_SESSION_SECRET=0123456789abcdef0123456789abcdef \
AKKV_COOKIE_SECURE=false \
AKKV_DATA_DIR=/tmp/akkvstore-data \
/tmp/akkvstore
```

访问：

```text
http://127.0.0.1:8080/admin/
```
