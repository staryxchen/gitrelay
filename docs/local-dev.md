# 本地构建与验证

需要 Go 1.26 或更新版本，不需要安装第三方依赖。

## 运行测试与编译

```sh
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/gitrelay ./gitrelay
```

测试不依赖公网 GitHub。集成测试会调用本机的 `git` 和 `git-http-backend`，在临时目录建一个 60 分支仓库跑真实克隆、增量 `fetch`、`gzip` 请求体转发和 `push` 拦截。

## 本地冒烟检查

用自定义回环端口启动二进制：

```sh
LISTEN_ADDR=127.0.0.1:18080 ./bin/gitrelay
```

另开终端依次验证健康检查、`CONNECT` 拦截和指标输出：

```sh
LISTEN_ADDR=127.0.0.1:18080 ./bin/gitrelay healthcheck
curl -i -X CONNECT http://127.0.0.1:18080/github.com:443
curl -s http://127.0.0.1:18080/metrics
```

正常结果：

- `healthcheck` 退出码为 `0`。
- `CONNECT` 返回 `405 Method Not Allowed`，响应头含 `Allow: GET, POST`。
- `/metrics` 返回 12 项 `gitrelay_*` 文本指标。
