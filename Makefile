.PHONY: build clean docker tidy test

# 本地交叉编译（Linux amd64）
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o iptv-proxy ./cmd/iptv-proxy

# 运行全部测试（Linux + gcc 环境可用 go test -race ./... 加竞态检测）
test:
	go test ./...

# 整理依赖
tidy:
	go mod tidy

# Docker 构建
docker:
	docker build -t iptv-udpproxy .

# 清理
clean:
	rm -f iptv-proxy
