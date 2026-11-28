# gitrelay

一个只读的 GitHub Git Smart HTTP 中继。在任意公开 GitHub 仓库地址前拼接你自己的代理域名，就可以加速 `git clone` 和 `git fetch`：

```sh
git clone https://relay.example.com/https://github.com/OWNER/REPO
git clone https://relay.example.com/https://github.com/OWNER/REPO.git
```

它只做只读 Git 转发，不是开放代理，不是 HTTP `CONNECT` 隧道，也不是 GitHub 网页镜像。

## 特点

- 兼容常见 GitHub 仓库写法。带或不带 `.git` 后缀、末尾斜杠、反代折叠后的 `/https:/github.com/...` 都能正常克隆。
- 兼容真实 Git 协议。支持 Smart HTTP v0 和 v2，支持多分支仓库拉取时 `git` 自动发出的 `Content-Encoding: gzip` 请求体。
- 只用 Go 标准库，零外部依赖。静态编译后约 6.6 MiB，空载内存约 6.7 MiB。
- 面向低配小内存机器裁剪。没有本地磁盘缓存，不写文件。用 `sync.Pool` 复用 32 KiB 缓冲逐块流式转发，不把整个 packfile 读进内存。上下游压缩数据原样透传，进程内不做加解压，既省 CPU 也防止压缩炸弹撑爆内存。
- 最小攻击面。只允许访问 `github.com:443` 两个只读路径。`GET /info/refs?service=git-upload-pack` 的 `301`、`302`、`307`、`308` 只有在 `Location` 仍指向 `https://github.com` 合法只读路径时，才改写为中继路径返回给 `git`。其他 `3xx` 返回 `502`。服务端不跟随重定向。`CONNECT` 和非 `GET`/`POST` 请求直接返回 `405`。上游连接前解析全部 DNS 并校验公网 IP，直连数字 IP 防止 DNS rebinding。进程强制只能绑定本机回环地址，公网访问控制交给前置反代。
- 自带零依赖监控。请求日志以 JSON 输出到标准输出，记录上游首字节耗时 `upstream_ms` 和错误分类 `error_kind`。回环 `/metrics` 暴露 12 个固定计数器和仪表值，没有动态标签，内存占用固定为 O(1)。

## 工作原理

`gitrelay` 只监听本机回环地址（默认 `127.0.0.1:8080`），TLS 和公网访问控制（源 IP 白名单、mTLS 或私网组网）由前置反代负责：

```text
git client
   |  HTTPS
   v
Caddy / Nginx (TLS + 访问控制 + 屏蔽 /healthz 和 /metrics)
   |  HTTP (127.0.0.1:8080, 关闭响应缓冲)
   v
gitrelay (方法拦截 -> 限流与并发槽 -> 路径规范化 -> 公网 IP 固定拨号)
   |  HTTPS (仅 github.com:443, 校验通过的 info/refs 重定向改写为中继路径, 压缩流原样透传)
   v
github.com
```

一次 `git clone` 会发两类请求，先 `GET /info/refs?service=git-upload-pack` 拿引用列表，再一次或多次 `POST /git-upload-pack` 协商并下载 packfile。`gitrelay` 校验 `owner` 和 `repo` 后，把上游地址统一重建为 `https://github.com/{owner}/{repo}.git/...` 再转发。仓库改名或转移时，上游对这次 `info/refs` 返回的 `301`、`302`、`307`、`308` 会在 `Location` 通过同一只读校验后，改写为 `/https://github.com/<owner>/<repo>.git/info/refs?service=git-upload-pack` 交回 `git`。`git` 再把后续 `POST /git-upload-pack` 发到新仓库。服务端不跟随重定向，也不缓存 POST 请求体。服务默认剥离请求中的 `Authorization`、`Cookie`、`Proxy-Authorization`、`Referer` 以及响应中的 `WWW-Authenticate` 和 `Set-Cookie`。显式设置 `ENABLE_AUTH_PASSTHROUGH=true` 后，服务端在零凭据存储的前提下仅向 `https://github.com` 透传客户端 `Authorization`，并在上游返回 `401` 时原样返回 `WWW-Authenticate`，支持私密仓库克隆与拉取（配置见 [服务器部署](docs/deploy.md)）。如果上游提前断流，服务端主动中止响应连接，避免客户端拿到残缺的包还以为下载完成。

## 不支持

- 除 `github.com` 以外的任何域名。
- `git push`（`git-receive-pack`）、Git LFS 和服务端凭据存储。
- Release 附件、源码 Archive 压缩包、`raw.githubusercontent.com` 文件和 GitHub 网页浏览。
- HTTP `CONNECT` 正向隧道，以及在服务端跟随上游重定向。除 `GET /info/refs?service=git-upload-pack` 上校验通过的 `301`、`302`、`307`、`308` 改写外，其他 `3xx` 返回 `502`。
- 本地磁盘缓存。

## 文档

- [开发与 AI 协作指南](AGENTS.md)
- [架构设计与核心取舍](docs/design.md)
- [服务器部署（systemd 与 Caddy）](docs/deploy.md)
- [配置、日志与指标参考](docs/operations.md)
