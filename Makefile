.PHONY: run build dist test vet fmt clean

# 开发运行（数据放在 ./kefu-data）
run:
	go run ./cmd/kefu

# 编译当前平台
build:
	go build -o kefu ./cmd/kefu

# 打包 Windows / macOS / Linux，输出到 dist/（VERSION=1.2.0 make dist）
dist:
	./scripts/build.sh

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

fmt:
	gofmt -w cmd internal web

clean:
	rm -rf kefu dist
