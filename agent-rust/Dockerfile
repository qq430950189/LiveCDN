# LiveCDN Agent - 多架构构建
# 仅用于 CI/CD 编译出 musl 静态二进制，不用在生产部署
# 生产环境: 直接下载编译好的二进制，不走 Docker

FROM rust:1.82 AS builder

RUN apt-get update && apt-get install -y musl-tools pkg-config

WORKDIR /app
COPY Cargo.toml Cargo.lock ./
RUN mkdir src && echo "fn main() {}" > src/main.rs && cargo build --release --target x86_64-unknown-linux-musl && rm -rf src

COPY . .
RUN touch src/main.rs && cargo build --release --target x86_64-unknown-linux-musl

# 输出阶段 - 只是把二进制拷出来
FROM alpine:3.19
COPY --from=builder /app/target/x86_64-unknown-linux-musl/release/livecdn-agent /livecdn-agent
# 这个镜像不会跑，只是用 docker cp 把二进制拷出来
CMD ["cp", "/livecdn-agent", "/output/livecdn-agent"]
