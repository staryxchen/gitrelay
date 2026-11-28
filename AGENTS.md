# AGENTS.md

本文件面向参与 `gitrelay` 开发的 AI 编程助手与维护者，定义不可破坏的架构边界、本地验证命令与提交流程。完整设计取舍见 [docs/design.md](docs/design.md)。

## 项目定位

`gitrelay` 是面向低配个人 VPS 的无状态 GitHub Git Smart HTTP 中继，仅使用 Go 标准库（零第三方依赖），通过流式转发加速受限网络环境下的 Git 操作。它不是开放代理、不是 HTTP `CONNECT` 隧道，也不是 GitHub 网页或文件镜像。

## 仓库结构

- `gitrelay/`：全部 Go 源码与测试（`package main`，含目标路径解析、公网 IP 固定拨号、流式处理器、指标与真实 `git-http-backend` 集成测试）。
- `deploy/`：原生 `systemd` 服务单元、宿主机 Caddy 2 配置模板与环境变量示例。
- `scripts/`：多架构静态构建（`build-release.sh`）、Release 发布（`publish-release.sh`）与公网/回环端到端校验（`verify-e2e.sh`）。
- `docs/`：使用、部署运维与架构决策文档。
- `.github/workflows/`：PR 门禁（`ci.yml`）、每日预发布（`nightly.yml`）与标签正式发布（`release.yml`）。

## 硬性约束（修改代码时不可破坏）

1. **零第三方依赖**：保持 `go.mod` 仅依赖 Go 标准库，未经明确讨论不引入外部 module。
2. **强制回环监听**：`LISTEN_ADDR` 必须校验为回环地址（`127.0.0.1`、`::1` 或 `localhost`）。不要在 Go 进程内实现登录鉴权或公网暴露，公网访问控制与 TLS 统一由前置反代负责。
3. **无状态流式转发**：禁止引入本地磁盘缓存。响应必须使用 `sync.Pool` 固定缓冲逐块流式写出并及时 `Flush`；上下游压缩流原样透传，禁止在进程内解压或重压请求/响应体。
4. **严格目标与协议边界**：
   - 仅放行校验通过的 `https://github.com:443` Git Smart HTTP 端点，默认保持只读（拒绝 `git-receive-pack`、Release、Archive、Raw 等路径）。
   - `CONNECT` 与非 `GET`/`POST` 请求必须在限流和并发槽之前直接返回 `405 Method Not Allowed` 与 `Allow: GET, POST`。
   - 上游拨号必须先解析全部 DNS 记录，拒绝任意非公网地址后直连数字 IP，防止 DNS rebinding 与 SSRF。
   - 禁止盲目跟随上游重定向，任何未来放开的跳转必须严格校验目标仍在 `https://github.com` 合法路径内。
5. **零服务端凭据**：绝不在服务端配置或存储常驻 GitHub PAT。后续私有仓库或写入能力必须通过显式关闭默认的开关启用客户端 `Authorization` / `WWW-Authenticate` 透传，且严禁把 `Authorization` 或 `Cookie` 写入任何日志或错误响应。
6. **O(1) 轻量观测**：结构化 JSON 日志不输出原始 URL、查询参数与请求头；`/metrics` 仅暴露固定 `atomic.Int64` 标量，禁止引入按仓库或 IP 展开的高基数标签。
7. **禁止泄露私有环境信息**：仓库代码、文档、脚本、测试、PR 与 Issue 中严禁出现真实个人域名、VPS IP、白名单出口 IP、自定义 SSH 端口或凭据，统一使用 `relay.example.com` 与 `<ALLOWED_CLIENT_IP>` 占位。

## 本地构建、测试与冒烟验证

依赖环境为 Go 1.26 或更新版本，以及本机 `git`（需包含 `git-http-backend`）。全部测试离线运行，不依赖公网 GitHub。

### 单元测试、竞态检测与静态构建

```sh
go vet ./...
go test -race -count=1 ./...
bash -n scripts/*.sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/gitrelay ./gitrelay
```

集成测试会在临时目录用本机 `git` 与 `git-http-backend` 创建 60 分支仓库，覆盖真实 `git clone`、增量 `git fetch`、`Content-Encoding: gzip` 请求体转发与 `git push` 拦截。

### 本地回环冒烟检查

使用自定义回环端口启动二进制：

```sh
LISTEN_ADDR=127.0.0.1:18080 ./bin/gitrelay
```

在另一终端执行基础检查：

```sh
LISTEN_ADDR=127.0.0.1:18080 ./bin/gitrelay healthcheck
curl -i -X CONNECT http://127.0.0.1:18080/github.com:443
curl -s http://127.0.0.1:18080/metrics
```

预期结果：

- `healthcheck` 退出码为 `0`。
- `CONNECT` 返回 `405 Method Not Allowed` 且包含 `Allow: GET, POST`。
- `/metrics` 返回固定 `gitrelay_*` Prometheus 文本指标。

若需直接验证含真实上游拉取的端到端链路，可在启动服务后执行：

```sh
./scripts/verify-e2e.sh http://127.0.0.1:18080
```

## 分支与 PR 规范

- 基于最新 `main` 拉取功能分支开发（命名使用 `feat/...`、`fix/...`、`docs/...`、`ci/...`），禁止直接推送 `main`。
- 每个 PR 保持单一可验证主题，合入前须通过全部 CI 检查。
- 若变更涉及新的安全边界、资源策略或协议行为取舍，必须在同一 PR 中同步更新 [docs/design.md](docs/design.md)。
