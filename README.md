# Gitrelay

为个人新加坡 2 核 / 4 GB VPS 设计的 **GitHub 公共文件下载与只读 Git 代理**。Go 单二进制、标准 Caddy 2 自动 TLS、Docker Compose 两个容器；不需要 Redis 或数据库。

**不是开放代理，不是 GitHub 网站镜像。** 仅访问公共仓库；不接收或转发 GitHub PAT、Cookie，不支持私有仓库、push、Git LFS、API、gist、网页浏览或任意 URL。客户端向代理提供的 Basic Auth 是本服务自己的凭据。

## 架构与资源

客户端 → Caddy HTTPS / HTTP/2 → Go 鉴权、全局限流与并发控制 → GitHub；完整公共文件可进入本地有界磁盘缓存。Git clone/fetch 流式透传、不缓存。

默认最多 8 个同时请求（包括下载和 Git），全局每秒 10 次、突发 20 次；超过立即返回 `429`，不建立排队队列。默认缓存 20 **GiB**、单对象最多 4 GiB、TTL 最多 24 小时。大于单对象上限或无 `Content-Length` 的下载仍可流式通过，只是不缓存。Go 默认内存软目标 384 MiB、容器硬上限 512 MiB；Caddy 硬上限 256 MiB。软目标不是内存保证，硬上限触发时可能 OOM 中断传输。

每路文件复制使用 32 KiB 缓冲，不将整个对象读入内存。没有全局下载/响应写入超时；连接、TLS 握手、响应头有超时，支持客户端取消。慢连接会占用并发槽，客户端应自行配置合理超时；个人使用可再配合防火墙源 IP 白名单。Git 请求体默认限制 32 MiB；这是上传协商包大小，不是仓库下载大小。

**磁盘建议至少预留 25–30 GiB 空闲空间供默认缓存使用，另留系统、镜像和日志空间。** VPS 流量/带宽账单仍由你承担：一次下载通常消耗一次上游入站和一次客户端出站，缓存只节约后续上游请求；本项目不提供带宽或月流量配额。先核对服务商的流量计费、GitHub 使用条款，避免公开分发滥用。

## 快速部署

需要：Linux VPS（amd64 或 arm64）、Docker Engine、Docker Compose v2、一个指向 VPS 的域名。先把 A 记录指向服务器公网 IPv4；只有确认 IPv6 可达时才添加 AAAA。开放 **TCP 80/443**（80 用于 HTTP 跳转及证书签发）。不要开放 8080，不要在已有服务占用 80/443 时直接启动。

```sh
git clone https://github.com/staryxchen/gitrelay.git
cd gitrelay
cp .env.example .env
mkdir -p secrets
chmod 700 secrets
umask 077
openssl rand -hex 32 > secrets/auth_password
chmod 644 secrets/auth_password
```

`secrets/` 的 700 权限限制宿主机目录访问；内部文件设为 644 是为了让容器中的非 root UID 10001 能读取 Compose 的只读文件挂载（本地 Compose 的 secret 不会自动加密或重新分配 UID）。不要把密码写入 `.env`、URL、命令参数、Git 配置或提交到版本库。密码为随机生成的 64 位十六进制字符串，使用你的密码管理器安全保存；服务启动会拒绝缺失或少于 16 字节的密码。

编辑 `.env`，至少填写：

| 参数 | 填写内容 |
|---|---|
| `DOMAIN` | 例如 `relay.example.com`，不带协议和路径 |
| `ACME_EMAIL` | 你的证书联系邮箱 |
| `AUTH_USERNAME` | 自定代理用户名，不含冒号/换行 |
| `AUTH_PASSWORD_PATH` | 默认 `./secrets/auth_password`，指向上述密码文件 |
| `CACHE_MAX_BYTES` | 根据实际空闲磁盘调整；默认 21474836480 |
| `CACHE_OBJECT_MAX_BYTES` | 默认 4294967296，必须给总上限留至少 16 KiB 元数据空间 |

```sh
docker compose config --quiet
docker compose build
docker compose run --rm --no-deps caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
docker compose up -d
docker compose ps
docker compose logs --tail=100 app caddy
```

Caddy 从 ACME 获取证书可能需要一些时间；DNS、防火墙或端口占用错误须先解决。标准 Caddy 不包含通用 `rate_limit` 指令，本配置没有依赖任何第三方插件。应用不发布宿主机端口，只在 Compose 网络中提供服务；这个网络必须能出站访问 GitHub 和公共 DNS。不要把应用直接暴露到公网，因为应用本身使用明文 HTTP。

本仓库不包含服务器登录信息，也不会自动连接或修改你的 VPS。上面是你在服务器执行的步骤，不代表已完成真实部署。

## 下载与安全鉴权

代理地址就是域名前加原始 HTTPS GitHub URL：

```text
https://relay.example.com/https://github.com/OWNER/REPO/releases/download/TAG/FILE
https://relay.example.com/https://github.com/OWNER/REPO/archive/refs/heads/main.zip
https://relay.example.com/https://github.com/OWNER/REPO/archive/refs/tags/v1.0.tar.gz
https://relay.example.com/https://raw.githubusercontent.com/OWNER/REPO/main/path/file
```

Release、archive 和 raw 入口只接受 GET/HEAD，**不接受查询参数**。有特殊字符的文件名需按 URL 正确转义，例如文件名里的 `?` 使用 `%3F`。页面上的资源、子模块地址、Release 中嵌入的链接不会自动重写。

curl 只传用户名会交互提示密码，避免 shell 历史和进程参数泄漏：

```sh
curl --user relay --fail --output FILE \
  'https://relay.example.com/https://github.com/OWNER/REPO/releases/download/TAG/FILE'
# 断点续传；MISS 透传 Range，HIT 在本地读取 Range。
curl --user relay --fail --continue-at - --output FILE \
  'https://relay.example.com/https://github.com/OWNER/REPO/releases/download/TAG/FILE'
# 查看响应头与缓存命中。
curl --user relay --head \
  'https://relay.example.com/https://github.com/OWNER/REPO/archive/refs/heads/main.zip'
```

GNU wget 支持交互密码提示（macOS 原生未预装 GNU wget）：

```sh
wget --user=relay --ask-password --continue --output-document=FILE \
  'https://relay.example.com/https://github.com/OWNER/REPO/releases/download/TAG/FILE'
```

自动化可使用独立的 `chmod 600` netrc 文件（curl `--netrc-file` / wget `--netrc`），只保存代理域名的凭据；这仍是磁盘明文机密，优先使用密码管理器集成。禁止把密码拼到 URL 或查询参数；不要使用 `curl --location-trusted`。上游跳转由应用处理，客户端无需 `-L` 来跟踪 GitHub 的签名下载 URL。

## 只读 Git clone / fetch

必须使用带 `.git` 的地址：

```sh
git clone 'https://relay.example.com/https://github.com/OWNER/REPO.git'
git -C REPO fetch --prune
```

首次收到 401 后 Git 会提示代理用户名和密码。建议使用系统安全凭据管理器：macOS 使用 `git config --global credential.helper osxkeychain`；Windows 使用已安装的 Git Credential Manager；Linux 使用已安装的 GCM 或 libsecret。**不要使用 `credential.helper store`**（明文存盘），不要设置把密码直接写入 `http.extraHeader` 或远程 URL 的方案。没有安全管理器时可不配置 helper，每次交互输入；`credential.helper cache` 是有时限的内存替代方案。

应用仅放行 `GET .../info/refs?service=git-upload-pack` 和 `POST .../git-upload-pack`；保留 `Git-Protocol`，不放行 `git-receive-pack`。Git 请求不缓存，不跟随重定向（仓库改名时更新远程地址）。Git LFS、子模块以及工具自行访问的 `github.com` URL 不会自动走此代理；需要时分别配置受支持的公共 Git 远程地址，不能把它当系统 HTTP CONNECT 代理。

## 缓存语义和边界

- 只缓存无 Range/条件头的完整 GET `200`、已知长度的公共对象。HEAD 不填充；MISS 的 Range/条件请求原样传给 GitHub，`206`、`304`、错误响应不入缓存。HIT 使用标准库处理 HEAD、Range、ETag、Last-Modified 和条件请求。
- 不缓存带 `private`、`no-store`、`no-cache`、Set-Cookie、Content-Encoding、不支持的缓存指令或 Vary 变体的响应。唯一接受的 Vary 是 `Accept-Encoding`，因为上游请求固定 `identity`。有些 GitHub 响应因此始终 MISS，这是有意的安全取舍，不应强行去掉其私有/变体标记。
- TTL 不超过填充时的配置上限及上游 `max-age` / `s-maxage` / `Expires` 的剩余有效期；过期不提供陈旧内容。无上游有效期的可缓存公共响应使用配置 TTL，因此 mutable raw/branch archive 最多可能旧一个 TTL。调整 TTL 只影响新填充，已有项沿用原过期时间。客户端 `Cache-Control: no-cache` 可绕过本地缓存，但不替换已有未过期项；需要立即清空或收紧已有期限时按下文停机维护。
- 对每个在写对象**提前预留完整体积**，总预算同时包含已提交文件、临时下载、每条 16 KiB 元数据和 4 KiB 块向上取整。未完成、客户端断连、上游截断都不提交；完整写入后 fsync 并原子 rename。并发相同对象仅一个请求填充，其余独立透传，不在内存积压。
- 使用 TTL + LRU 淘汰，最多 4096 条记录。正在读取的缓存文件不淘汰，防止 unlink 后仍占用磁盘却不计入预算；空间无法预留时只透传。启动清理临时/损坏/过期文件，缓存目录使用进程锁，**不能多实例共享写入同一目录**。
- 字节预算不是文件系统配额：目录、inode、文件系统日志、块尺寸大于 4 KiB 的额外开销及 Docker 日志不在预算内。必须保留磁盘余量；严格磁盘隔离请给 Docker 数据目录设置独立分区/配额。磁盘错误记录 ERROR 并计入错误指标，尽可能继续完成当前下载，不静默假装缓存成功。缓存不是备份，掉电后不承诺保留最后一次提交。

响应中 `X-Gitrelay-Cache: HIT|MISS` 可诊断缓存；对客户端统一使用 `Cache-Control: private, no-store`，避免代理鉴权内容再次落入公共中间缓存。服务只面向命令行下载，响应带 CSP sandbox 和 nosniff，不应作为可执行网页托管域名使用。

## SSRF 与凭据边界

公开入口仅 `github.com` 的 Release / archive / 只读 Git 路径和 `raw.githubusercontent.com` 文件路径。上游强制 HTTPS、默认 443，不允许 URL userinfo、自定端口、任意域名或整个 `*.githubusercontent.com`。

每次重定向重新校验，第 5 次跳转时拒绝；入口域名仍受下载路径约束。额外仅允许 archive 所需的 `codeload.github.com` 指定 archive 路径及 Release 所需的 `release-assets.githubusercontent.com` / `objects.githubusercontent.com` 的 `github-production-release-asset` 路径（objects 还支持旧的精确前缀 `github-production-release-asset-2e65be`）。GitHub 更改资源域名时会失败关闭，需审查后更新精确白名单，不应改成通配。

每次新建上游连接解析全部 DNS 地址；发现私网、回环、链路本地、组播、文档/保留地址等即拒绝，只连接已检查的数字 IP，同时保持正常 TLS 主机名验证，防 DNS rebinding。不读取环境中的 `HTTP_PROXY` / `HTTPS_PROXY`，避免绕开检查。操作系统 DNS 解析器本身仍是可信基础设施，应使用可信 DNS 和正常网络配置。

客户端 Authorization、Cookie、Proxy-Authorization、Referer、不在白名单的头和 hop-by-hop 头不会透传。应用不记录完整 URL、请求头、密码或 GitHub 签名重定向；错误用哈希对象键关联。Caddy 未启用额外访问日志，避免重复记录敏感 URL。全局令牌桶也限制未认证请求，但不是分布式攻击防护：公开域名仍建议防火墙仅允许自己的出口 IP，并定期轮换密码。

## 运维与配置

```sh
docker compose ps
docker compose logs --tail=100 app
curl --user relay --fail https://relay.example.com/metrics
docker compose exec app /usr/local/bin/gitrelay healthcheck
docker compose exec app du -sh /var/cache/gitrelay
docker system df
```

`/metrics` 使用同一 Basic Auth；提供请求数、命中/MISS、错误、拒绝、响应字节、当前请求和含临时写入的预留磁盘字节等 Prometheus 文本指标，无需部署 Prometheus 才能使用。`/healthz` 是容器内部无鉴权的存活检查，Caddy 公网入口返回 404；它不检查 GitHub 连通性、缓存写入或可用磁盘，因此不能替代磁盘告警。访问日志是 JSON（方法、状态、字节、时长），不包含原始 URL。

应用日志和 Caddy 日志各最多约 3 × 10 MB；默认不记录每次下载的明文 URL。GitHub 429/403 等上游响应原样返回状态码，不自动无限重试；首先检查上游限流、网络与请求是否受支持。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Compose 固定使用此地址；内建 healthcheck 使用 127.0.0.1:8080 |
| `AUTH_USERNAME` | 必填 | 代理自己的用户名 |
| `AUTH_PASSWORD_FILE` | Compose secret 路径 | 文件优先方案；原生运行也可单独设置 `AUTH_PASSWORD`，二者互斥 |
| `CACHE_DIR` | `/var/cache/gitrelay` | 专用持久化缓存目录，UID 10001 须可写 |
| `CACHE_MAX_BYTES` | `21474836480` | 包含临时写入与元数据的预算，正整数 |
| `CACHE_OBJECT_MAX_BYTES` | `4294967296` | 单对象填充上限，加 16 KiB 元数据并向上取整到 4 KiB 后须不超过总预算；过大只透传 |
| `CACHE_TTL` | `24h` | 正时长，最多 `720h`；上游可进一步缩短 |
| `MAX_CONCURRENT` | `8` | 不排队；下载、Git 和 metrics 共用并发槽 |
| `REQUESTS_PER_SECOND` | `10` | 全局请求速率，不按 IP 信任 X-Forwarded-For |
| `REQUEST_BURST` | `20` | 全局突发额度；healthz 不消耗 |
| `GIT_REQUEST_MAX_BYTES` | `33554432` | Git POST 上行请求体限制，最大 1 GiB |

限流/并发数配置范围 1–10000，这不是建议在 4 GB 机器配置上万并发。配置无效、机密缺失、缓存无法初始化或缓存目录被另一实例锁定时会清楚报错并退出，不以匿名模式启动。

更新与密码轮换：

```sh
git pull --ff-only
docker compose build --pull
docker compose pull caddy
docker compose up -d --force-recreate
# 修改密码文件后，强制重建 app 使其重新读取，旧下载会中断：
docker compose up -d --force-recreate app
```

正常停止给正在进行的传输 30 秒退出窗口，超时后中断；升级前尽量等下载完成。Caddy 的 `caddy_data` 卷保存证书，不要随意删除；不要执行 `docker compose down -v` 来清缓存，那也会删证书。

如需清缓存，先停止 **app**，在维护容器内查看目录并只删除 `.cache` / `.tmp-*` 文件，然后再启动；不要运行中删文件或删除整个 Docker 数据目录：

```sh
docker compose stop app
docker compose run --rm --no-deps --entrypoint sh app -c \
  'find /var/cache/gitrelay -maxdepth 1 -type f \( -name "*.cache" -o -name ".tmp-*" \) -print -delete'
docker compose up -d app
```

宿主机若已有 HTTPS 反代，可以自行接入，但要保持应用不公网暴露、TLS、无正文缓冲、合理响应头超时、无总下载时限及凭据日志脱敏；本仓库默认只维护上述 Caddy 拓扑。

## 本地开发与验证

使用仍受维护的 Go 1.26（建议最新补丁版）或更新版本，只有标准库依赖；支持 Linux 和 macOS（文件锁使用 Unix flock）。首次运行必须设置认证和可写缓存目录：

```sh
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -o bin/gitrelay .
# 设置真实密码文件路径，不把密码值写进 shell 命令：
AUTH_USERNAME=relay AUTH_PASSWORD_FILE=/absolute/path/to/password \
  CACHE_DIR=./cache ./bin/gitrelay
```

测试使用本地受控 TLS 上游，不依赖 GitHub 公网；覆盖入口/方法白名单、重定向、DNS/IP 防护、凭据隔离、认证/限流、Git 请求边界、Range/条件语义、缓存预留/淘汰/重启/TTL、并发、断流/取消、磁盘故障和大文件传输。测试中的本地传输替身只存在于 `_test.go`，生产没有“允许私网”的后门配置。

Dockerfile / Compose / Caddyfile 还应在你的实际 Docker 环境运行前述配置检查与启动步骤。单元测试不证明 VPS DNS、证书签发、出站网络、实际 GitHub clone 互操作、镜像构建或磁盘容量正确；请在部署后先用小公共文件和小公共仓库做验收。
