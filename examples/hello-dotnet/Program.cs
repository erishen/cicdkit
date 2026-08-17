// 零依赖 .NET HTTP 服务，用于演示 cicdkit 的构建 / 发布 / 探针闭环。
// 使用 ASP.NET Core 内建的 Kestrel（原生支持在 Linux 上监听 0.0.0.0，
// 而框架自带的 HttpListener 在 Unix 上仅支持 localhost/127.0.0.1 前缀，
// 绑定 0.0.0.0 会抛 HttpListenerException(50) "The request is not supported"）。
// 读取 PORT 与 APP_VERSION 环境变量。

var builder = WebApplication.CreateBuilder(args);
var port = Environment.GetEnvironmentVariable("PORT") ?? "8080";
var version = Environment.GetEnvironmentVariable("APP_VERSION") ?? "dev";

// 显式绑定到 0.0.0.0，使容器外部（Docker 端口映射 / k3s NodePort）可达。
builder.WebHost.UseUrls($"http://0.0.0.0:{port}");

var app = builder.Build();

app.MapGet("/", () => Results.Text($"Hello from hello-dotnet (version {version})\n",
    contentType: "text/plain; charset=utf-8"));

app.MapGet("/health", () => Results.Ok());

app.Run();
