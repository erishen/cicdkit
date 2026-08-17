require 'socket'

# 零依赖 Ruby HTTP 服务，用于演示 cicdkit 的构建 / 发布 / 探针闭环。
# 仅用标准库（socket），读取 PORT 与 APP_VERSION 环境变量（由平台 Dockerfile 的 ENV 注入）。
PORT = (ENV['PORT'] || '8080').to_i
VERSION = ENV['APP_VERSION'] || 'dev'

puts "hello-ruby #{VERSION} listening on :#{PORT}"

server = TCPServer.new('0.0.0.0', PORT)
loop do
  # 每连接一个线程，足够演示；真实服务可换 Puma / Falcon 等应用服务器。
  Thread.start(server.accept) do |sock|
    request_line = sock.gets
    sock.close and next if request_line.nil?

    body = "Hello from hello-ruby (version #{VERSION})\n"
    sock.print "HTTP/1.1 200 OK\r\n"
    sock.print "Content-Type: text/plain; charset=utf-8\r\n"
    sock.print "Content-Length: #{body.bytesize}\r\n"
    sock.print "Connection: close\r\n\r\n"
    sock.print body
    sock.close
  end
end
