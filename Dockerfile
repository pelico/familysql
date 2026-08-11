FROM golang:1.23-alpine AS builder
# 安装 CGO 编译工具链与 sqlite-dev，用于 better-sqlite3 或 go-sqlite3 的 CGO 依赖
RUN apk add --no-cache gcc musl-dev sqlite-dev
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# 关键：开启 CGO 且增加 FTS5 编译标签
RUN CGO_ENABLED=1 go build -tags "sqlite_fts5" -o main .

# 运行阶段
FROM alpine:latest
RUN apk add --no-cache sqlite-libs
WORKDIR /app
# 拷贝编译好的二进制文件与前端静态资源
COPY --from=builder /app/main .
COPY --from=builder /app/public ./public/
EXPOSE 18080
CMD ["./main"]
