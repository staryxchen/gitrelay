# 服务器部署（systemd 与 Caddy）

`gitrelay` 不内置登录鉴权，强制只监听本机回环地址。公网访问控制必须在前端反向代理或防火墙完成，例如固定源 IP 白名单、mTLS 客户端证书或私网组网。不要把 `127.0.0.1:8080` 无保护地暴露到公网。

## 0. 安装 Caddy（可选）

如果服务器已经在 80 和 443 端口运行了 Caddy 或 Nginx，跳过本节。

Debian 和 Ubuntu 可通过官方软件源安装 Caddy 2：

```sh
apt-get update
apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
apt-get update
apt-get install -y caddy
```

其他发行版参考 [Caddy 官方安装文档](https://caddyserver.com/docs/install)。安装后确认防火墙放行 TCP 80 和 443，且域名 A 记录已指向服务器公网 IPv4。

## 1. 安装二进制与 systemd 服务

先在本地按 [本地构建与验证](local-dev.md) 编译出 `bin/gitrelay`，连同 `deploy/gitrelay.service` 上传到 Linux 服务器后执行。没有 Go 环境的服务器可先执行[第 5 节](#5-从-github-release-校验并升级)的步骤 1 至 4 将预编译二进制校验并安装到 `/usr/local/bin/gitrelay`（随后执行 `rm -rf "${TMP_DIR}"` 清理临时目录），再跳过下面第一行，直接安装 `gitrelay.service` 并启用服务：

```sh
install -m 0755 gitrelay /usr/local/bin/gitrelay
install -m 0644 gitrelay.service /etc/systemd/system/gitrelay.service
systemctl daemon-reload
systemctl enable --now gitrelay
systemctl status gitrelay --no-pager
(
  if [ -f /etc/gitrelay/gitrelay.env ]; then set -a; . /etc/gitrelay/gitrelay.env; fi
  ok=0
  for _ in $(seq 1 20); do
    if /usr/local/bin/gitrelay healthcheck; then ok=1; break; fi
    sleep 0.5
  done
  [ "$ok" -eq 1 ]
)
```

服务默认监听 `127.0.0.1:8080`，不需要任何环境变量就能启动。如果需要调整限流或监听端口，参考 `deploy/gitrelay.env.example` 创建 `/etc/gitrelay/gitrelay.env`，然后执行 `systemctl restart gitrelay`。

`deploy/gitrelay.service` 已配置非 root 动态用户、只读根目录和低配资源上限（`GOMEMLIMIT=128MiB`、`MemoryHigh=128M`、`MemoryMax=192M`、`CPUQuota=100%`、`Nice=5`、`TasksMax=64`）。

## 2. 接入宿主机 Caddy 2

参考 `deploy/gitrelay.caddy`，把站点块追加到 `/etc/caddy/Caddyfile` 末尾：

- 将 `relay.example.com` 换成你自己的域名或子域名。建议独立子域名，不要和其他代理混在同一个站点块里。
- 将 `<ALLOWED_CLIENT_IP>` 替换为允许访问的固定出口 IP，多个 IP 用空格分隔。
- 站点块用 `handle @allowed { ... } handle { abort }` 实现默认拒绝，非白名单 IP 会被直接断开。
- `/healthz` 和 `/metrics` 对公网返回 `404`，只在本机调试查看。
- `flush_interval -1` 必须保留，防止反代缓冲大仓库 packfile 或拖慢 Git 进度输出。如果前端用的是 Nginx，对应设置 `proxy_buffering off;` 并用 `allow <IP>; deny all;` 限制来源 IP。

改完后先备份并校验，不要直接重启线上 Caddy：

```sh
cp /etc/caddy/Caddyfile /etc/caddy/Caddyfile.bak
caddy validate --config /etc/caddy/Caddyfile
systemctl reload caddy
```

如果线上 `/etc/caddy/Caddyfile` 原本写了自定义的 `:80` 重定向块，重载前用 `caddy adapt --config /etc/caddy/Caddyfile --pretty` 确认新域名的路由和证书策略正常。

如果宿主机 Caddy 安装了第三方 `listener_wrappers` 插件，热重载后检查 443 端口监听数量：

```sh
ss -tlnp | grep :443
```

正常情况下 `:443` 只应有一条监听记录。如果同一个 Caddy 进程同时出现多条 `*:443` 监听，说明热重载未释放旧套接字，新旧配置会在 `SO_REUSEPORT` 下随机分流，导致新域名 TLS 握手偶发 `tlsv1 alert internal error`（`no certificate available`）。确认 `caddy validate` 通过且现有插件状态已持久化到磁盘后，执行一次 `systemctl restart caddy` 清理旧套接字，再复查 `ss -tlnp | grep :443`。

## 3. 限制 journald 日志容量（建议）

`gitrelay` 与宿主机 Caddy 都通过标准输出把日志交给 `systemd-journald`。系统默认按根分区容量的 10% 且封顶 4 GiB 轮转，小容量 VPS 建议显式收紧上限：

```sh
mkdir -p /etc/systemd/journald.conf.d
cat <<'EOF' > /etc/systemd/journald.conf.d/99-limit.conf
[Journal]
SystemMaxUse=200M
SystemMaxFileSize=32M
MaxRetentionSec=14day
EOF
systemctl restart systemd-journald
journalctl --vacuum-size=200M --vacuum-time=14d
journalctl --disk-usage
```

## 4. 验收

在白名单机器上测试克隆，确认不带 `.git` 和带 `.git` 都能正常工作：

```sh
git clone https://relay.example.com/https://github.com/OWNER/REPO
git -C REPO fetch --prune
```

同时确认三项安全边界：

- 白名单机器访问 `https://relay.example.com/healthz` 和 `https://relay.example.com/metrics` 返回 `404`。
- 非白名单公网 IP 连接 `https://relay.example.com` 被直接断开。
- 尝试 `git push` 被拒绝。

## 5. 从 GitHub Release 校验并升级

已按第 1 节安装 `gitrelay.service` 的无 Go 环境 Linux 服务器，可直接下载 GitHub Release 构建的静态二进制完成原地升级。

1. 指定目标版本 `VERSION`（正式版填 `v0.1.0` 等标签，每日构建填 `nightly`），映射当前机器架构，并将二进制与 `SHA256SUMS` 下载到独立临时目录：

```sh
VERSION="v0.1.0"
case "$(uname -m)" in
  x86_64) ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
BIN="gitrelay-linux-${ARCH}"
BASE_URL="https://github.com/staryxchen/gitrelay/releases/download/${VERSION}"
TMP_DIR="$(mktemp -d)"

curl -fsSL -o "${TMP_DIR}/${BIN}" "${BASE_URL}/${BIN}"
curl -fsSL -o "${TMP_DIR}/SHA256SUMS" "${BASE_URL}/SHA256SUMS"
```

2. `SHA256SUMS` 同时包含 `amd64` 与 `arm64` 条目。用 `awk` 精确提取当前架构单行后交给 `sha256sum -c -` 校验，缺失、重复或字节损坏都会返回非零退出码：

```sh
(
  cd "${TMP_DIR}" &&
  awk -v bin="${BIN}" '$2 == bin || $2 == "*" bin { line = $0; n++ } END { if (n == 1) print line; else exit 1 }' SHA256SUMS | sha256sum -c -
)
```

3. 升级前不要对线上旧二进制执行 `/usr/local/bin/gitrelay --version`，早期版本未实现 `--version` 参数，直接运行会尝试启动服务。只对刚通过校验的新二进制赋予执行权限并预检版本输出：

```sh
chmod 0755 "${TMP_DIR}/${BIN}"
"${TMP_DIR}/${BIN}" --version
```

4. 若旧二进制已存在，先备份为 `/usr/local/bin/gitrelay.bak`。随后用 `install` 写入同目录临时文件 `/usr/local/bin/gitrelay.new`，再用 `mv -f` 在同一文件系统内原子替换 `/usr/local/bin/gitrelay`，避免直接覆盖运行中进程触发 `ETXTBSY` 或留下半写文件：

```sh
if [ -f /usr/local/bin/gitrelay ]; then
  install -m 0755 /usr/local/bin/gitrelay /usr/local/bin/gitrelay.bak
fi
install -m 0755 "${TMP_DIR}/${BIN}" /usr/local/bin/gitrelay.new
mv -f /usr/local/bin/gitrelay.new /usr/local/bin/gitrelay
```

5. `gitrelay.service` 使用 `Type=simple`，`systemctl restart gitrelay` 会在进程完成监听端口绑定前返回。若存在 `/etc/gitrelay/gitrelay.env`，子 shell 会先加载其中的自定义 `LISTEN_ADDR`。重启服务，核对生效版本并轮询回环健康检查，确认进程存活后再清理临时目录：

```sh
systemctl restart gitrelay
/usr/local/bin/gitrelay --version
(
  if [ -f /etc/gitrelay/gitrelay.env ]; then set -a; . /etc/gitrelay/gitrelay.env; fi
  ok=0
  for _ in $(seq 1 20); do
    if /usr/local/bin/gitrelay healthcheck; then ok=1; break; fi
    sleep 0.5
  done
  [ "$ok" -eq 1 ]
)
systemctl is-active --quiet gitrelay
rm -rf "${TMP_DIR}"
```

6. 如果升级后验证失败，用备份文件原子回滚并重启服务：

```sh
install -m 0755 /usr/local/bin/gitrelay.bak /usr/local/bin/gitrelay.rollback
mv -f /usr/local/bin/gitrelay.rollback /usr/local/bin/gitrelay
systemctl restart gitrelay
(
  if [ -f /etc/gitrelay/gitrelay.env ]; then set -a; . /etc/gitrelay/gitrelay.env; fi
  ok=0
  for _ in $(seq 1 20); do
    if /usr/local/bin/gitrelay healthcheck; then ok=1; break; fi
    sleep 0.5
  done
  [ "$ok" -eq 1 ]
)
systemctl is-active --quiet gitrelay
```
