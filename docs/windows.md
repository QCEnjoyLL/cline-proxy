# Cline Proxy Windows 使用说明

## 下载与启动

- 普通 Intel / AMD 64 位 Windows 电脑选择 `windows-amd64.zip`。
- Windows ARM 设备选择 `windows-arm64.zip`。
- 解压到一个固定、可写的目录，运行 `cline-proxy.exe`，无需安装 Go 或 Docker。
- 在浏览器中打开 <http://127.0.0.1:3457/admin/>。程序需要持续运行，关闭终端窗口会停止服务。

默认管理员用户名为 `admin`。未设置密码时，程序会在启动日志中显示自动生成的管理员密码。

也可在 PowerShell 中设置管理员账号和密码后启动：

```powershell
$env:ADMIN_USER = 'admin'
$env:ADMIN_PASSWORD = '请换成自己的强密码'
.\cline-proxy.exe -port 3457
```

这些环境变量只对当前 PowerShell 及其启动的程序生效；exe 不会自动读取 Docker Compose 使用的 `.env` 文件。不要使用 `-start` 启动下载的发行包，该选项用于从源码重新编译。

## 添加账号与更新

进入管理后台添加账号、选择模型并配置 API Key。账号、凭据及其他运行数据保存在 exe 所在目录。

更新时先退出程序，再用新版 exe 替换旧版，保留同目录中的 `.cline-accounts.json`、`.cline-credentials.json` 和 `override.md`（如果存在）。请勿将账号配置或凭据文件分享给他人。

查看当前版本：

```powershell
.\cline-proxy.exe -version
```

## 校验下载文件

Release 中的 `SHA256SUMS.txt` 包含各 exe 和 ZIP 文件的校验值。使用 PowerShell 计算下载文件的 SHA256，与对应行比较：

```powershell
Get-FileHash -LiteralPath .\下载的文件名.zip -Algorithm SHA256
```

项目说明：<https://github.com/QCEnjoyLL/cline-proxy>

发行版与源码：<https://github.com/QCEnjoyLL/cline-proxy/releases>
