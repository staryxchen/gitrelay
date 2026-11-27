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

先在本地按 [本地构建与验证](local-dev.md) 编译出 `bin/gitrelay`，连同 `deploy/gitrelay.service` 上传到 Linux 服务器后执行：

```sh
install -m 0755 gitrelay /usr/local/bin/gitrelay
install -m 0644 gitrelay.service /etc/systemd/system/gitrelay.service
systemctl daemon-reload
systemctl enable --now gitrelay
systemctl status gitrelay --no-pager
/usr/local/bin/gitrelay healthcheck
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

## 3. 验收

在白名单机器上测试克隆，确认不带 `.git` 和带 `.git` 都能正常工作：

```sh
git clone https://relay.example.com/https://github.com/OWNER/REPO
git -C REPO fetch --prune
```

同时确认三项安全边界：

- 白名单机器访问 `https://relay.example.com/healthz` 和 `https://relay.example.com/metrics` 返回 `404`。
- 非白名单公网 IP 连接 `https://relay.example.com` 被直接断开。
- 尝试 `git push` 被拒绝。
