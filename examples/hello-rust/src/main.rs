use std::io::{Read, Write};
use std::net::TcpListener;

// 零依赖 Rust HTTP 服务，用于演示 cicdkit 的构建 / 发布 / 探针闭环。
// 读取 PORT 与 APP_VERSION 环境变量（由平台 Dockerfile 的 build-arg 注入）。
// 仅用标准库 std::net::TcpListener 实现，无需任何外部 crate。

fn main() {
    let port = std::env::var("PORT").unwrap_or_else(|_| "8080".to_string());
    let version = std::env::var("APP_VERSION").unwrap_or_else(|_| "dev".to_string());
    let addr = format!("0.0.0.0:{}", port);

    let listener = TcpListener::bind(&addr).expect("bind failed");
    println!("hello-rust {} listening on :{}", version, port);

    // 顺序处理连接即可演示；真实服务应改为多线程 / 异步。
    for stream in listener.incoming() {
        match stream {
            Ok(mut s) => {
                // 只需读取请求行即可决定响应，不做完整 HTTP 解析。
                let mut buf = [0u8; 1024];
                let _ = s.read(&mut buf);

                let body = format!("Hello from hello-rust (version {})\n", version);
                let resp = format!(
                    "HTTP/1.1 200 OK\r\n\
                     Content-Type: text/plain; charset=utf-8\r\n\
                     Content-Length: {}\r\n\
                     Connection: close\r\n\r\n{}",
                    body.len(),
                    body
                );
                let _ = s.write_all(resp.as_bytes());
                let _ = s.flush();
            }
            Err(_) => continue,
        }
    }
}
