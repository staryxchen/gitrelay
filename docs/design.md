# 架构设计与核心取舍

本文档以轻量 ADR（Architecture Decision Record）形式记录 `gitrelay` 的关键设计选择。每条记录明确写出结论、取舍原因、接受的代价与重估前提，作为后续功能演进与 AI 协作的长期护栏。

---

## ADR-1 仅做 Git Smart HTTP 中继，不做通用代理与文件镜像

- **决策**：仅转发校验通过的 `https://github.com/<owner>/<repo>.git` Git Smart HTTP 端点，明确拒绝 HTTP `CONNECT`、Release 附件、Archive 源码包、`raw.githubusercontent.com` 与网页浏览。
- **原因与取舍**：支持任意下载或 `CONNECT` 会立刻把个人小服务变成开放代理或高带宽大文件镜像，既放大 SSRF、滥用和滥发流量风险，也容易把低配 VPS 带宽和内存打满。把协议面收窄到 Git Smart HTTP，使目标解析、请求体校验和内容类型约束都可以精确白名单化。
- **接受的代价**：无法直接用同一服务加速 `curl` 下载 GitHub Release 二进制或 Raw 脚本。
- **重估条件**：如果未来确实需要代理 Git LFS 等大文件对象存储（见路线图远期调研项），需先单独完成域名白名单、流控和单文件上限的安全边界设计。

## ADR-2 采用零第三方依赖的 Go 标准库，而非 Rust

- **决策**：使用 Go 标准库实现全部核心功能，保持 `go.mod` 零外部依赖，而不采用 Rust 重写。
- **原因与取舍**：
  - 线上在 2 vCPU、3.6 GiB 内存的 VPS 上实测，Go 版静态二进制约 `6.9 MiB`，常驻内存约 `5 到 7 MiB`；真实仓库克隆耗时中超过 90% 消耗在跨国网络 RTT 与 GitHub 上游响应，本机 CPU 与 GC 耗时几乎可忽略。
  - Go 标准库原生覆盖 HTTP/1.1、HTTP/2、TLS、自定义拨号、`slog` 与 `-race` 检测，后续安全修补只需升级单一 Go 工具链重编；且 `CGO_ENABLED=0` 可在 CI 中秒级产出 `linux/amd64` 与 `linux/arm64` 静态二进制。
  - Rust 标准库不包含异步运行时、HTTP 栈与 TLS，同等能力需引入 `tokio`、`hyper`、`rustls` 等数十到上百个 crate，依赖审计、大版本升级与 musl 静态交叉编译的长期维护成本远高于 Go。
- **接受的代价**：进程常驻内存比 Rust 略高约 `2 到 3 MiB`，且存在亚毫秒级 GC 停顿。
- **重估条件**：当部署目标变为 `64 到 256 MiB` 极低内存嵌入式/路由器设备，或并发规模扩大到数千长连接、出现 CPU 或尾延迟瓶颈时，再评估 Rust。

## ADR-3 无状态流式转发与低配 VPS 资源裁剪

- **决策**：不设本地磁盘缓存，不落盘临时 packfile；使用 `sync.Pool` 复用 32 KiB 缓冲逐块流式转发并立即 `Flush`；上下游 `gzip` 请求与响应原样透传，不在进程内解压。
- **原因与取舍**：低配 VPS 磁盘与内存都有限，磁盘缓存会引入淘汰策略、锁竞争、残缺缓存清理和冷热膨胀等一整套状态复杂度。流式转发配合全局并发槽（默认 `8`）、令牌桶限流（默认 `10 req/s`、突发 `20`）、请求体上限（默认 `32 MiB`）与 `systemd` 内存配额（`GOMEMLIMIT=128MiB`、`MemoryHigh=128M`、`MemoryMax=192M`），可以把最坏情况资源占用锁死在可控区间，同时避免压缩炸弹攻击。
- **接受的代价**：多个客户端重复克隆同一大仓库时每次都会回源 GitHub，无法复用本地磁盘缓存。
- **重估条件**：当团队级高频重复拉取同一超大仓库、且部署机器具备独立大容量存储与足够内存时，再评估是否单独引入外部缓存层。

## ADR-4 强制回环监听，公网访问控制收口到宿主机反代

- **决策**：`gitrelay` 强制校验 `LISTEN_ADDR` 必须为回环地址（`127.0.0.1`、`::1` 或 `localhost`），删除进程内 Basic Auth；TLS 终止、固定源 IP 白名单（或 mTLS）以及屏蔽公网 `/healthz` 和 `/metrics` 全部交由宿主机 Caddy 或 Nginx 处理。
- **原因与取舍**：将认证与网络隔离放在前置反代，一方面复用宿主机现成的 80/443 证书自动续期与连接终止能力，另一方面把非白名单流量挡在进入 Go 进程之前（Caddy `handle { abort }` 直接断开），减少应用层攻击面与重复配置。强制回环校验则从代码层面防止误配导致服务裸奔公网。
- **接受的代价**：无法脱离反向代理直接向公网提供 HTTPS 服务。
- **重估条件**：若未来需要在无反代的纯独立环境中直接暴露公网端口，需重新设计内置 TLS 与认证模块后再放开监听校验。

## ADR-5 零服务端凭据原则与标准 HTTP 认证透传

- **决策**：服务端绝不保存或配置常驻 GitHub PAT。私密仓库只读拉取通过默认关闭的环境变量 `ENABLE_AUTH_PASSTHROUGH`（默认 `false`）启用。开启后，进程仅在目标通过校验且指向 `https://github.com` 时透传客户端 `Authorization` 请求头，且仅在上游返回 `401 Unauthorized` 时原样透传 `WWW-Authenticate` 响应头以触发本地 `git` 凭据协商，同时配合宿主机 Caddy `format filter` 删除日志中的 `Authorization`、`Cookie`、`Proxy-Authorization` 与 `Set-Cookie` 字段。后续若支持 `git push`（`git-receive-pack`），将继续复用同一零服务端凭据边界。明确拒绝以下四种替代凭据注入方案：
  - **服务端常驻 PAT（环境变量或密钥文件）**：一旦 VPS 或白名单内任意客户端失陷，攻击者即可获取该 PAT 覆盖的全部仓库权限，违背按客户端最小权限隔离原则。
  - **自定义非标准 HTTP 请求头（如 `X-GitHub-Token`、`X-Relay-Token`）**：破坏标准 Git Smart HTTP `401` 协商流程，绕过反向代理和日志组件的默认敏感头脱敏规则，并迫使服务端解析和改写凭据。
  - **目标 GitHub URL 内嵌凭据（`/https://user:PAT@github.com/...`）**：`target.go` 在 `u.User != nil` 时直接以 `400` 拒绝，防止密钥进入请求路径后残留在反向代理日志、报错输出和终端历史中。
  - **路径前缀携带 Token（`/<TOKEN>/https://github.com/...`）**：将密钥暴露在 URL 路径中，带来日志、监控链路和 shell 历史泄漏风险，同时破坏直接拼接中继域名的标准 Git URL 形式。
- **原因与取舍**：标准 `Authorization` 与 `WWW-Authenticate` 按需透传让 `gitrelay` 保持无状态纯转发，不解析凭据内容，也不落盘任何密钥。虽要求每台客户端机器按中继域名配置本地 `credential.helper`，但换来了服务端零密钥落盘、按客户端最小权限隔离以及全链路日志零凭据泄漏。
- **接受的代价**：客户端访问私密仓库（以及未来可能的推送）时需要在本地为中继域名配置 Git 凭据辅助器，无法实现免客户端鉴权的全局代拉私有库。
- **重估条件**：仅在封闭私网内作为受控 CI 专用只读拉取代理、且明确接受机器级凭据托管风险时，才考虑独立派生模式。

## ADR-6 原生 systemd 静态二进制部署，不引入 Docker

- **决策**：采用单个静态编译二进制配合加固后的 `systemd` 单元和宿主机反代部署，通过 GitHub Release 产物与 `SHA256SUMS` 校验完成原子替换升级，不维护 Docker Compose 方案。
- **原因与取舍**：低配 VPS 通常未预装 Docker，引入容器引擎会常驻额外内存与磁盘开销，并增加宿主机端口与网络命名空间排查复杂度。Go 静态二进制天然无运行时依赖，配合 `DynamicUser=yes`、`ProtectSystem=strict`、`PrivateTmp=yes`、`NoNewPrivileges=yes` 与 cgroup 资源上限即可获得清晰隔离。
- **接受的代价**：多机规模化编排不如容器镜像统一，需要依赖宿主机 `systemd`。
- **重估条件**：如果未来部署目标转向 Kubernetes 或多节点容器平台，再按需补充极简 scratch/distroless 镜像构建流程。

## ADR-7 O(1) 轻量观测，不引入外部监控栈

- **决策**：请求日志以单行 JSON 输出到标准输出并交由 `systemd-journald` 收集与轮转，严禁记录原始 URL、查询参数与请求头；回环 `/metrics` 仅暴露固定 `atomic.Int64` 标量，不带仓库名、IP 或路径等动态标签。
- **原因与取舍**：低配 VPS 不适合常驻 Prometheus、Grafana 或日志采集代理。固定标量指标保证了无论运行多久、访问多少仓库，进程内存占用都保持 O(1)；`journald` 配合 `LogRateLimit` 与 `SystemMaxUse` 即可防止异常流量写满磁盘。
- **接受的代价**：无法直接按单个仓库维度聚合历史流量曲线，只能从近期日志按需过滤统计。
- **重估条件**：当服务转为多人共享并需要按仓库或用户做长期计费/审计报表时，再接入外部时序与日志存储。

## ADR-8 只改写校验过的 info/refs 重定向

- **决策**：`CheckRedirect` 返回 `http.ErrUseLastResponse`。`parseRedirectTarget` 决定能否把这次重定向交回客户端。只接受当前请求是 `GET /info/refs?service=git-upload-pack`，状态码是 `301`、`302`、`307` 或 `308`，且响应里只有一个 `Location`。`Location` 必须是 `https://github.com/...`，或以单个 `/` 开头的源站相对路径。相对路径通过拼接 `https://github.com` 还原，不调用 `url.ResolveReference`，路径里的 `..` 保持原样并在校验时拒绝。还原后的地址走与客户端请求相同的只读路径校验。通过后，服务用相同状态码返回根相对 `Location`，值为 `/https://github.com/<owner>/<repo>.git/info/refs?service=git-upload-pack`，并丢弃上游响应体最多 4 KiB。响应保留 `X-Content-Type-Options`、`Content-Security-Policy`、`Referrer-Policy`，以及值为 `private, no-store` 的 `Cache-Control`。不转发 `Set-Cookie` 和 `WWW-Authenticate`。自环、跨主机、`http`、用户信息、显式端口、片段、额外查询参数、`POST`、多值 `Location` 和其他 `3xx` 都返回 `502`。`error_kind` 为 `upstream_redirect`，自环为 `upstream_redirect_loop`。日志和错误正文都不包含 `Location` 或 `Authorization`。
- **原因与取舍**：仓库改名或转移后，GitHub 在 `info/refs` 上返回重定向。若在服务端跟随，后续 `POST /git-upload-pack` 必须先缓存请求体才能重放。上限是 `GIT_REQUEST_MAX_BYTES`，默认 32 MiB，最大 1 GiB。这会打破 32 KiB 流式转发，也会超出 `MemoryMax=192M`。把校验后的 `Location` 交回 `git` 后，客户端打印 `warning: redirecting to ...`，并把后续 `POST` 直接发到新仓库。规范化后的 URL 与当前 `info/refs` 相同时立即返回 `502`。两个仓库互相把 `info/refs` 指到对方时，由 `git` 最多跟随 20 次重定向和现有令牌桶的 `429` 终止。进程不保存跳转链，也不把 `Authorization` 发到其他主机。
- **接受的代价**：`git` 会显示重定向警告，远程 URL 变为中继上的新仓库路径。`POST` 上的 `3xx` 直接返回 `502`，服务端不重放请求体。Git 2.35 在 `401` 早于 `301` 时不会更新仓库基址。私有仓库要先在旧路径返回重定向，再在新路径返回 `401`。
- **重估条件**：如果 GitHub 改为只在 `POST /git-upload-pack` 上重定向，或必须隐藏客户端的重定向警告，再评估有上限的请求体缓冲。在此之前不在服务端跟随重定向。
