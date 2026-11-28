# 配置、日志与指标参考

## 环境变量

全部环境变量都是可选的，未设置时直接使用默认值：

| 环境变量 | 默认值 | 校验规则 |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:8080` | 必须是本机回环地址（`127.0.0.1`、`::1` 或 `localhost`），端口 `1` 到 `65535` |
| `MAX_CONCURRENT` | `8` | 全局并发槽上限，整数 `1` 到 `10000`，超出返回 `429` |
| `REQUESTS_PER_SECOND` | `10` | 全局令牌桶每秒补充数，整数 `1` 到 `10000` |
| `REQUEST_BURST` | `20` | 全局令牌桶突发容量，整数 `1` 到 `10000` |
| `GIT_REQUEST_MAX_BYTES` | `33554432` | `POST /git-upload-pack` 压缩后请求体上限，默认 32 MiB，最大 1 GiB |
| `ENABLE_AUTH_PASSTHROUGH` | `false` | 布尔值（Go `strconv.ParseBool`，如 `true`、`false`、`1`、`0`），空值或非法值拒绝启动。开启后仅向 `https://github.com` 透传 `Authorization`，并在上游 `401` 时原样返回 `WWW-Authenticate` |

## 常用排查命令

在服务器上直接查看进程资源、实时日志和累计流量，不需要额外部署监控服务：

```sh
systemctl status gitrelay --no-pager
journalctl -u gitrelay -n 50 --no-pager
curl -s http://127.0.0.1:8080/metrics
```

`deploy/gitrelay.service` 对 `journald` 输出做了限速（`LogRateLimitIntervalSec=30s`、`LogRateLimitBurst=200`）。查看当前日志总占用与生效上限可执行：

```sh
journalctl --disk-usage
systemd-analyze cat-config systemd/journald.conf | grep -vE '^[[:space:]]*(#|$)'
```

如需收紧磁盘上限与保留天数，按 [服务器部署](deploy.md) 中的步骤配置 `/etc/systemd/journald.conf.d/99-limit.conf`。

如果配置新域名并重载宿主机 Caddy 后出现偶发 TLS 握手失败（`tlsv1 alert internal error`），优先用 `ss -tlnp | grep :443` 核查是否残留多条 `*:443` 监听套接字，处理方式见 [服务器部署](deploy.md)。

## 日志字段

服务启动时向标准输出写一条 `service started` JSON 日志，包含 `listen`、`version`、`commit`、`built` 和布尔字段 `auth_passthrough`。

每条请求向标准输出写一行 `request` JSON 日志。所有请求都包含 `method`、`status`、`req_bytes`、`resp_bytes` 和 `duration_ms`。

通过路径校验的 Git 请求会额外记录三个字段：

- `action` 为 `info_refs` 或 `upload_pack`。
- `repo` 为校验后的 `owner/repo`。
- `upstream_ms` 为请求发出到收到 GitHub 响应头的毫秒数，对比 `duration_ms` 可以分清慢在 GitHub 上游还是客户端链路。

发生拒绝或失败时附加 `error_kind`。请求日志与错误日志均不输出 `Authorization`、`Cookie`、原始 URL、查询参数和请求头。

## `/metrics` 指标

`/metrics` 仅在回环地址访问，输出标准 Prometheus 文本格式。所有指标都是固定的 `atomic.Int64` 标量，没有按仓库或 IP 展开的动态标签，内存占用固定：

| 指标名 | 类型 | 含义 |
|---|---|---|
| `gitrelay_requests_total` | counter | 收到的总请求数 |
| `gitrelay_info_refs_total` | counter | `GET /info/refs` 请求数 |
| `gitrelay_upload_pack_total` | counter | `POST /git-upload-pack` 请求数 |
| `gitrelay_active_requests` | gauge | 当前正在处理的请求数 |
| `gitrelay_request_bytes_total` | counter | 上行 `git-upload-pack` 累计字节数 |
| `gitrelay_response_bytes_total` | counter | 下行响应累计字节数 |
| `gitrelay_upstream_errors_total` | counter | 上游失败、5xx、重定向拒绝和断流次数 |
| `gitrelay_client_aborts_total` | counter | 客户端主动取消或断连次数 |
| `gitrelay_rejected_rate_limit_total` | counter | 触发令牌桶限流返回 `429` 次数 |
| `gitrelay_rejected_concurrency_total` | counter | 触发并发上限返回 `429` 次数 |
| `gitrelay_rejected_method_total` | counter | 拒绝 `CONNECT` 或非法 HTTP 方法返回 `405` 次数 |
| `gitrelay_uptime_seconds` | gauge | 进程已运行时长（秒） |
