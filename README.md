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
4. 为程序配置：

```bash
export KV_URL=https://kv.example.com
export KV_TOKEN=kv_xxxxxxxxxxxxxxxxxxxxxxxx
export KV_PROJECT=my-app
```

5. 使用公开 API 创建 table 或直接 PUT 第一个 key。

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

公开 API 前缀：

```text
/v1
```

除健康检查和管理后台外，所有公开 API 都需要 Project Token：

```http
Authorization: Bearer <project-token>
```

每个 Token 都拥有对应 project 内全部 table 和 key 的读写删除权限。Token 不应暴露给不受信任的浏览器端或客户端；生产环境必须使用 HTTPS。

示例：

```bash
curl "$KV_URL/v1/$KV_PROJECT/tables" \
  -H "Authorization: Bearer $KV_TOKEN"
```

约定：

- JSON 时间为 Unix 毫秒时间戳。
- 错误响应为 JSON。
- PUT 和 DELETE 使用 `X-Updated-At` 表示客户端最后修改时间。
- 服务端不自动比较时间，不合并 value，也不判断业务冲突。
- 冲突处理由客户端通过 `X-KV-Condition` 完成。

## 接口总览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/v1/{project}/tables` | 列出 table |
| `POST` | `/v1/{project}/tables` | 创建空 table |
| `GET` | `/v1/{project}/tables/{table}` | 获取 table 元信息 |
| `DELETE` | `/v1/{project}/tables/{table}` | 删除 table 和其外部文件 |
| `GET` | `/v1/{project}/tables/{table}/kv/{key}` | 读取 value |
| `HEAD` | `/v1/{project}/tables/{table}/kv/{key}` | 获取 key 元信息 |
| `PUT` | `/v1/{project}/tables/{table}/kv/{key}` | 创建或覆盖 value |
| `DELETE` | `/v1/{project}/tables/{table}/kv/{key}` | 写入删除墓碑 |
| `GET` | `/v1/{project}/tables/{table}/records` | 分页查询记录 |

所有接口都支持 CORS 预检 `OPTIONS`。

## Table API

### 列出 table

```http
GET /v1/{project}/tables
```

响应：

```json
{
  "items": [
    {
      "name": "users",
      "keyCount": 12,
      "createdAt": 1760000000000,
      "updatedAt": 1760000005000
    }
  ]
}
```

### 创建 table

```http
POST /v1/{project}/tables
Content-Type: application/json
```

```json
{"name":"users"}
```

成功返回 `201 Created`，并带 `Location` 响应头。

规则：

- table 名必须匹配 `[a-z0-9][a-z0-9_-]{0,31}`。
- 同名 table 返回 `409 table_exists`。
- 首次 PUT 到不存在的 table 时会自动创建 table。

### 获取和删除 table

```http
GET /v1/{project}/tables/{table}
DELETE /v1/{project}/tables/{table}
```

删除 table 会删除：

- table 元数据
- table 内全部 key
- 对应外部大文件

此操作不可恢复。

## KV API

### 写入或覆盖 value

```http
PUT /v1/{project}/tables/{table}/kv/{key}
Authorization: Bearer <token>
Content-Type: application/json
X-Updated-At: 1762490000123
```

请求体就是 value 原始字节，不会强制解析为 JSON。

成功返回：

```http
204 No Content
ETag: "<content-hash>"
X-Value-Size: 27
X-Created-At: 1762490000000
X-Updated-At: 1762490000123
```

规则：

- `X-Updated-At` 必须是正整数 Unix 毫秒时间戳。
- 未提供 `X-Updated-At` 返回 `400 invalid_updated_at`。
- 未提供 `X-KV-Condition` 时直接覆盖，不比较新旧时间。
- 写入新 key 成功时创建记录；覆盖已有 key 保留原始 `createdAt`。
- `Content-Type` 会随 value 保存。

### 读取 value

```http
GET /v1/{project}/tables/{table}/kv/{key}
```

成功返回原始字节和保存的 `Content-Type`，同时带：

```text
ETag
Content-Length
Last-Modified
X-Created-At
X-Updated-At
```

支持标准 HTTP：

- `HEAD`
- `Range`
- `If-None-Match`

### 删除 value

```http
DELETE /v1/{project}/tables/{table}/kv/{key}
X-Updated-At: 1762490000124
```

成功返回 `204 No Content`。

DELETE 不物理删除记录，而是写入删除墓碑：

- 普通 GET 返回 `404 not_found`
- 普通 records 查询默认不返回
- `includeDeleted=true` 时返回，且 `deleted: true`
- 对不存在的 key 执行 DELETE 也会创建墓碑

## Key 规则

- 最长 1024 字节。
- 必须是合法 UTF-8。
- 不能包含控制字符。
- 可以包含 `/`，例如 `dir/sub/file`。
- 在 URL 中必须正确编码 `?`、`#`、空格等字符。
- 不能以 `/` 开头或结尾。
- 不能包含连续斜杠。
- 不能包含 `.` 或 `..` 路径段。
- `a.b`、`file.txt` 这类普通点号允许。

## 原子条件写入

`PUT` 和 `DELETE` 都支持：

```http
X-KV-Condition: {"all":[...]}
```

所有条件按 AND 计算。条件与写入在同一个事务中执行。

示例：

```json
{
  "all": [
    {
      "field": "updatedAt",
      "op": "lt",
      "value": 1762490000123
    },
    {
      "field": "contentType",
      "op": "eq",
      "value": "application/json"
    },
    {
      "field": "value",
      "op": "startsWith",
      "value": "{\"version\":"
    }
  ]
}
```

支持的字段和操作符：

| 字段 | 类型 | 操作符 |
| --- | --- | --- |
| `exists` | bool | `eq`、`ne` |
| `deleted` | bool | `eq`、`ne` |
| `updatedAt` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `createdAt` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `size` | int64 | `eq`、`ne`、`lt`、`lte`、`gt`、`gte` |
| `contentType` | string | `eq`、`ne`、`lt`、`lte`、`gt`、`gte`、`startsWith`、`endsWith` |
| `contentHash` | string | `eq`、`ne`、`lt`、`lte`、`gt`、`gte`、`startsWith`、`endsWith` |
| `value` | UTF-8 bytes | `eq`、`ne`、`lt`、`lte`、`gt`、`gte`、`startsWith`、`endsWith` |

记录不存在时：

- `exists=false`
- `deleted=false`
- 数值字段按 `0` 计算
- 字符串和 `value` 按空值计算

删除墓碑：

- `exists=true`
- `deleted=true`

条件不成立返回：

```http
412 Precondition Failed
X-Created-At: 1762490000000
X-Updated-At: 1762490000122
```

```json
{
  "error": {
    "code": "precondition_failed",
    "message": "key \"user:1001\" precondition failed"
  },
  "current": {
    "key": "user:1001",
    "exists": true,
    "deleted": false,
    "createdAt": 1762490000000,
    "updatedAt": 1762490000122,
    "contentType": "application/json",
    "contentHash": "...",
    "contentLength": 27
  }
}
```

限制：

- `X-KV-Condition` 最大 8 KiB。
- `value` 条件的比较值最大 8 KiB。
- 比较完整 value 时优先使用 `contentHash`，避免传输大内容。

Last-Write-Wins 示例：

```bash
curl -X PUT "$KV_URL/v1/$KV_PROJECT/tables/users/kv/user:1001" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Updated-At: 1762490000123" \
  -H 'X-KV-Condition: {"all":[{"field":"updatedAt","op":"lt","value":1762490000123}]}' \
  --data-binary '{"name":"张三","vip":true}'
```

## 查询记录

```http
GET /v1/{project}/tables/{table}/records
```

参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `prefix` | 空 | key 前缀匹配 |
| `pattern` | 空 | SQLite GLOB 模式，例如 `watchLater/*` |
| `updatedAfter` | 空 | 客户端 `updatedAt` 下限，包含边界 |
| `updatedBefore` | 空 | 客户端 `updatedAt` 上限，包含边界 |
| `includeDeleted` | `false` | 是否返回删除墓碑 |
| `includeValues` | `false` | 是否返回 value |
| `limit` | `100` | 每页数量，最大 `500` |
| `offset` | `0` | 分页偏移 |

`updatedAfter` 和 `updatedBefore` 支持：

- Unix 毫秒时间戳
- RFC3339 时间

响应：

```json
{
  "items": [
    {
      "key": "user:1001",
      "createdAt": 1760000000000,
      "updatedAt": 1760000005000,
      "deleted": false,
      "contentType": "application/json",
      "size": 26,
      "contentHash": "...",
      "valueBase64": "eyJhZGRlZEF0IjoiLi4uIn0=",
      "valueText": "{\"addedAt\":\"...\"}"
    }
  ],
  "total": 1,
  "nextOffset": 1,
  "hasMore": false
}
```

`includeValues=true` 时：

- 所有 value 都返回 `valueBase64`。
- 常见文本类型且内容为合法 UTF-8 时额外返回 `valueText`。
- 单页原始 value 总量超过 32 MiB 返回 `413 query_value_limit`。

大文件应使用单个 key 的 GET 接口下载，不要在 records 中批量拉取。

`updatedAt` 完全由客户端提交。时间回拨、错误时钟和迟到的离线写入可能造成覆盖顺序不符合业务预期；时间过滤不能替代全量快照。

## 错误格式

普通错误：

```json
{
  "error": {
    "code": "not_found",
    "message": "key 不存在"
  }
}
```

常见错误码：

| HTTP | code | 说明 |
| --- | --- | --- |
| `400` | `invalid_updated_at` | `X-Updated-At` 缺失或非法 |
| `400` | `invalid_condition` | 条件格式、字段或操作符非法 |
| `400` | `invalid_query` | records 查询参数非法 |
| `400` | `invalid_request` | table 创建等请求体格式非法 |
| `400` | `invalid_key` | key 格式非法 |
| `400` | `invalid_table` | table 名称非法 |
| `401` | `unauthorized` | Token 缺失、错误或 project 不匹配 |
| `404` | `not_found` | key 或接口不存在 |
| `404` | `table_not_found` | table 不存在 |
| `405` | `method_not_allowed` | HTTP 方法不支持 |
| `409` | `table_exists` | table 已存在 |
| `412` | `precondition_failed` | 原子条件不成立 |
| `413` | `too_large` | value 超过上限 |
| `413` | `query_value_limit` | 单页查询 value 总量超过上限 |

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
X-KV-Condition
X-Updated-At
```

暴露的响应头：

```text
ETag
Content-Length
Last-Modified
X-Value-Size
X-Created-At
X-Updated-At
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
curl -X POST "$KV_URL/v1/$KV_PROJECT/tables" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary '{"name":"users"}'
```

写入 JSON：

```bash
curl -X PUT "$KV_URL/v1/$KV_PROJECT/tables/users/kv/user:1001" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Updated-At: $MODIFIED_AT" \
  --data-binary '{"name":"张三","vip":true}'
```

读取：

```bash
curl "$KV_URL/v1/$KV_PROJECT/tables/users/kv/user:1001" \
  -H "Authorization: Bearer $KV_TOKEN"
```

查询并包含 value：

```bash
curl "$KV_URL/v1/$KV_PROJECT/tables/users/records?prefix=user:&includeValues=true&limit=100" \
  -H "Authorization: Bearer $KV_TOKEN"
```

删除：

```bash
curl -X DELETE "$KV_URL/v1/$KV_PROJECT/tables/users/kv/user:1001" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "X-Updated-At: 1762490000124"
```

上传二进制：

```bash
curl -X PUT "$KV_URL/v1/$KV_PROJECT/tables/files/kv/report.pdf" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -H "Content-Type: application/pdf" \
  -H "X-Updated-At: 1762490000125" \
  --data-binary @report.pdf
```

下载：

```bash
curl "$KV_URL/v1/$KV_PROJECT/tables/files/kv/report.pdf" \
  -H "Authorization: Bearer $KV_TOKEN" \
  -o report.pdf
```

## 设计边界

- `updatedAt` 由客户端维护，服务端不校验时钟准确性。
- 服务端不处理业务冲突，只执行客户端提供的原子条件。
- 不提供完整历史版本链。
- 不提供 TTL。
- 不提供原子数值递增。
- 不提供批量写事务。
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
