# wechat-token-manager

## 项目用途

本项目是一个微信公众号 `access_token` 中控服务，用于统一管理多个公众号的 Token 获取、缓存、自动刷新与授权访问。

适用场景：
- 多个业务系统需要共享微信公众号 Token
- 不希望各业务方分别请求微信接口
- 需要统一控制 Token 缓存、刷新频率、签名鉴权和配置热重载

核心能力：
- 支持多个公众号账号统一管理
- Token 缓存到 Redis
- Token 过期前自动刷新
- 分布式锁避免多实例重复刷新
- RSA 签名鉴权保护接口访问
- 配置文件热重载
- 启动时预热全部公众号 Token

---

## 技术栈

- Go 1.21+
- Gin
- Redis
- Resty
- Zap
- fsnotify

---

## 目录说明

```text
.
├── config/
│   ├── config.yaml
│   ├── config.yaml.example
│   ├── rsa_public_key.pem
│   └── rsa_private_key.pem
├── main.go
├── main_test.go
├── go.mod
└── go.sum
```

说明：
- `config/config.yaml`：实际运行配置
- `config/config.yaml.example`：配置示例
- `config/rsa_public_key.pem`：接口验签使用的 RSA 公钥
- `config/rsa_private_key.pem`：签名方使用的 RSA 私钥

注意：私钥仅供签名方保管，不应暴露给调用方。

---

## 配置说明

配置文件默认路径：

```yaml
config/config.yaml
```

也可以启动时通过参数指定：

```bash
go run . -config /your/path/config.yaml
```

如果指定了 `-config`，程序会使用该文件，并且该配置中引用的相对文件路径也会以该配置文件所在目录为基准解析。

如果不指定，默认使用项目目录下的：

```bash
config/config.yaml
```

### 配置示例

```yaml
server:
  port: ":8080"
  read_timeout: "5s"
  write_timeout: "10s"

redis:
  addr: "localhost:6379"
  password: ""
  db: 0
  prefix: "wx_token"

wechat:
  token_api: "https://api.weixin.qq.com/cgi-bin/token"
  refresh_ahead: 3600
  auto_refresh_interval: "10m"

api:
  rsa_public_key: "rsa_public_key.pem"
  sign_expire_hours: 2
  clients:
    - appid: "your_client_appid"
      secret: "your_client_secret"
      status: "enabled"

logger:
  level: "info"
  encoding: "json"
  output_path: "stdout"   # stdout、单文件路径，或日志目录（如 logs）
  max_size: 100            # 仅在 output_path 为单文件路径时生效
  max_backup: 7            # 仅在 output_path 为单文件路径时生效
  max_age: 30              # 仅在 output_path 为单文件路径时生效
  compress: false          # 仅在 output_path 为单文件路径时生效
  retention_days: 7        # 仅在 output_path 为目录时生效，0 表示不清理旧日志

accounts:
  - appid: "wx123"
    appsecret: "secret123"
```

### 字段说明

#### server
- `port`：服务监听地址
- `read_timeout`：HTTP 读超时
- `write_timeout`：HTTP 写超时

#### redis
- `addr`：Redis 地址
- `password`：Redis 密码
- `db`：Redis DB 编号
- `prefix`：Redis Key 前缀

#### wechat
- `token_api`：微信获取 Token 接口地址
- `refresh_ahead`：Token 提前刷新秒数
- `auto_refresh_interval`：自动刷新轮询周期

#### api
- `rsa_public_key`：RSA 公钥配置，仅支持两种写法：
  1. 直接填写 PEM 公钥内容
  2. 填写公钥文件路径（推荐）
- `sign_expire_hours`：签名过期时间，单位小时
- `clients`：允许访问本服务的调用方列表
  - `appid`：调用方标识
  - `secret`：调用方签名密钥
  - `status`：`enabled` / `disabled`

#### logger
- `level`：日志级别，支持 `debug` / `info` / `warn` / `error`
- `encoding`：日志格式，支持 `json` / `console`
- `output_path`：支持三种写法：
  1. `stdout`：输出到控制台（默认）
  2. 单文件路径：如 `logs/app.log`，按 lumberjack 规则滚动
  3. 目录路径：如 `logs`，按天写入文件，文件名格式为 `wechat-token-manager-YYYY-MM-DD.log`
- `max_size` / `max_backup` / `max_age` / `compress`：仅在 `output_path` 为单文件路径时生效
- `retention_days`：仅在 `output_path` 为目录时生效，保留最近多少天的日志文件；`0` 或负数表示不自动清理

#### accounts
微信公众号列表：
- `appid`：公众号 AppID
- `appsecret`：公众号 AppSecret

---

## RSA 密钥说明

项目内已生成一组示例密钥：

- 公钥：`config/rsa_public_key.pem`
- 私钥：`config/rsa_private_key.pem`

服务端只使用公钥验签。

调用方生成签名时使用私钥，签名原文格式为：

```text
appid|client_secret|timestamp
```

签名算法：
- RSA PKCS#1 v1.5
- SHA-256
- 签名结果再做 Base64 编码

请求头要求：
- `X-AppId`
- `X-Signature`
- `X-Timestamp`

---

## 启动方式

### 1. 使用默认配置启动

```bash
go run .
```

默认会读取项目目录下的：

```text
config/config.yaml
```

### 2. 指定配置文件启动

```bash
go run . -config config/config.yaml
```

或：

```bash
./wechat-token-manager -config /your/path/config.yaml
```

指定 `-config` 后：
- 程序使用该配置文件启动
- 配置中的相对路径（如 `api.rsa_public_key`、日志目录等）都相对该配置文件所在目录解析

### 3. 编译运行

```bash
go build -o wechat-token-manager .
./wechat-token-manager -config config/config.yaml
```

---

## 接口说明

### 1. 健康检查

```http
GET /api/v1/health
```

返回示例：

```json
{
  "code": 200,
  "msg": "ok",
  "data": {
    "status": "healthy",
    "ready": true,
    "account_count": 1,
    "client_count": 1,
    "hot_reload": "enabled",
    "timestamp": "2026-05-28T12:00:00Z",
    "version": "v1.0.0"
  }
}
```

### 2. 获取公众号 Token

```http
GET /api/v1/wechat/token?appid=wx123
```

请求头：

```http
X-AppId: your_client_appid
X-Timestamp: 1716888888
X-Signature: base64-encoded-signature
```

返回示例：

```json
{
  "code": 200,
  "msg": "ok",
  "data": {
    "appid": "wx123",
    "access_token": "ACCESS_TOKEN",
    "expires_in": 7200,
    "expire_time": "2026-05-28T12:00:00Z",
    "last_update": "2026-05-28T10:00:00Z",
    "refresh_ahead": 3600
  }
}
```

---

## 运行机制说明

### 1. 启动预热
启动时会预加载全部公众号 Token。

- 预热成功：服务进入就绪状态
- 部分预热失败：服务降级启动，后续由自动刷新补齐

### 2. Token 获取流程
1. 先查 Redis
2. 若 Token 仍有效，直接返回
3. 若失效，尝试获取分布式锁
4. 拿到锁的实例请求微信接口并刷新 Token
5. 刷新结果写回 Redis

### 3. 自动刷新
- 仅主节点执行自动刷新
- 主节点通过 Redis 锁选举
- 多实例部署时可避免重复刷新

### 4. 配置热重载
程序会监听配置文件所在目录，在配置文件变更时自动热重载。

当前允许热重载的配置项包括：
- `accounts`
- `api.clients`
- `api.sign_expire_hours`
- `wechat.refresh_ahead`
- `wechat.auto_refresh_interval`
- `wechat.token_api`

以下配置变更不会热重载，需重启服务：
- `server`
- `redis`
- `logger`
- `api.rsa_public_key`

---

## 开发与测试

### 格式化

```bash
gofmt -w main.go main_test.go
```

### 测试

```bash
go test ./...
```

---
## 部署建议

- Redis 使用独立 DB 或唯一前缀隔离环境
- 生产环境不要把私钥提交到代码仓库
- 公钥建议使用文件路径配置，避免把 PEM 直接塞进 YAML
- 多实例部署时保持配置一致
- 线上如需落盘日志，建议将 `logger.output_path` 配置为目录，例如 `logs`
- 建议配合进程管理器或容器平台运行

---

## 常见问题

### 1. 公钥解析失败
请检查：
- `api.rsa_public_key` 是否填写为正确 PEM 公钥内容
- 或是否指向存在的公钥文件路径
- 不要把私钥内容填到 `rsa_public_key`

### 2. Redis 连接失败
请检查：
- `redis.addr`
- `redis.password`
- `redis.db`
- Redis 服务是否可达

### 3. 签名验证失败
请检查：
- `X-AppId` 是否存在于 `api.clients`
- `X-Timestamp` 是否过期或超出允许时钟偏差
- `X-Signature` 是否由正确私钥签出
- 签名原文是否严格使用 `appid|client_secret|timestamp`

### 4. 指定配置文件后找不到公钥文件
如果 `rsa_public_key` 使用相对路径，该路径是相对**配置文件所在目录**解析的，不是相对当前执行目录。

### 5. 如何让日志写入 logs 目录并按天分文件
将配置改为：

```yaml
logger:
  output_path: "logs"
  retention_days: 7
```

此时日志会写入配置文件所在目录下的 `logs/`，并按天生成文件，例如：

```text
logs/wechat-token-manager-2026-05-28.log
```

程序会在写入新日期日志文件时，自动清理超出 `retention_days` 的旧日志文件。