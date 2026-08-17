import os
import http.server
import socketserver

# 零依赖 Python HTTP 服务，用于演示 cicdkit 的构建 / 发布 / 探针闭环。
# 仅用标准库（http.server / socketserver），读取 PORT 与 APP_VERSION 环境变量
# （由平台 Dockerfile 的 ENV 注入）。
PORT = int(os.environ.get("PORT", "8080"))
VERSION = os.environ.get("APP_VERSION", "dev")


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path in ("/health", "/healthz"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"healthy"}')
            return
        body = f"Hello from hello-python (version {VERSION})\n".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    # 抑制默认访问日志，保持输出干净
    def log_message(self, *args):
        pass


if __name__ == "__main__":
    socketserver.TCPServer.allow_reuse_address = True
    with socketserver.TCPServer(("", PORT), Handler) as httpd:
        print(f"hello-python {VERSION} listening on :{PORT}")
        httpd.serve_forever()
