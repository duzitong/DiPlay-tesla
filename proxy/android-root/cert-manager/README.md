# V50 证书管理页（独立 root 服务）

只管理手动 DNS-01 证书续期，不管理配对码，不保存 Oray API key。
首次车机配对仍使用电脑 ADB / scrcpy。此服务不修改 APK、热点或路由。

## 手机使用

1. iPhone 连接 V50 热点，打开配置中的 `origin`（示例端口 9444），输入独立管理密码。
2. 剩余 35 天内，查看并明确接受当前 Let’s Encrypt 条款，生成 TXT。
3. 到 Oray 更新 `_acme-challenge.carplay` TXT，其他记录保持不变。
4. 返回点击“检查 DNS 并签发证书”。两个公共 DNS 都看到正确值后才提交验证。
5. 确认“安装证书并重启网关”。连接可能短暂中断；验证新证书失败会尝试恢复旧证书。

关闭页面不会取消正在进行的后台任务。申请状态、账号密钥、待安装证书保存在设备，
重启后可继续。新证书复用当前 TLS 私钥，因此只需原子替换证书链，避免两个文件更新
一半导致密钥不匹配。账号使用另一把密钥，全部文件仅 root 可读。

**这不是无人值守续期**：每次仍需手动更新 Oray TXT。没有推送或日历提醒，请自行设提醒。
证书有效期以页面显示为准，不承诺固定 90 天。不要重复提前申请，避免 CA 频率限制。

## 构建和检查

使用 Go 1.27.3 或更新兼容版本。在本目录运行 `go test ./...`、`go vet ./...`。
设置 `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` 后构建 `go build -trimpath -o cert-manager .`。
可用 `go test -c` 交叉编译测试程序，推到 Android 临时目录运行，验证 Unix 权限。
另运行仓库 `AGENTS.md` 规定的完整 Gradle 检查和代理 Node 测试。

## 一次部署

复制 `config.example.json` 到已忽略的 `.private/https-gateway/manager-config.json`，
设置真实域名、明确的私有 IPv4 监听地址和**独立端口**、现有证书路径。
本目录附带 shell 脚本针对 `/data/local/diplay-https` 网关、`br0` 的 `192.168.0.1`，
其他布局需先相应调整脚本。`reloadScript` 只能是固定 root 所有脚本，不能由网页指定。

将构建程序、本目录 `.sh` 和本地配置（设备端命名 `config.json`）推到
`/data/local/tmp/diplay-cert-manager-stage`。文件必须是 LF 换行，临时目录设置 700。
用户亲自在终端以 root 执行该目录的 `install.sh`。安装器拒绝覆盖已有安装，
不重启现有 Caddy，不创建 ACME 账号/订单；仅生成随机管理密码并启动独立管理服务。
安装失败的部分目录需先检查后人工处理，不能盲目重复安装。

密码在 `/data/local/diplay-cert-manager/state/admin-password`，请用户自己通过 root
终端读取后保存到密码管理器；不要贴进聊天、URL、日志或 Git。页面不存 localStorage /
sessionStorage / Cookie，刷新页面需重新输入。丢失密码需通过 USB root 维护，不提供公网重置。

服务依赖系统根证书，脚本设置 `SSL_CERT_DIR` 为 Android Conscrypt 和系统 CA 路径。
不禁用 CA 校验。DNS 查询使用 1.1.1.1 / 8.8.8.8；网络屏蔽公共 DNS 或 Let's Encrypt 时
会明确报错，不跳过验证。服务只绑定指定热点地址，不监听 0.0.0.0；不要端口映射到公网。
密码保护不替代网络隔离：同一热点客户端可以访问登录页，应使用可信设备和热点密码。

## 到期与故障恢复

**管理页与业务入口共用现有域名证书。必须提前续期。证书过期后，iPhone 浏览器可能
拒绝连接，本实现不提供明文 HTTP 密码入口，也不要求忽略证书警告。此时需要 USB/ADB
或另一个预先建立的可信加密维护通道恢复，不能保证过期后仍无需电脑。**

安装前备份保存在私有状态目录的 `previous-fullchain.pem`，已签发链在
`pending-fullchain.pem`。检测到中断安装时，下次确认安装会先恢复备份，并要求再次确认。
回滚失败会明确提示人工处理，不报成功。网关重载只停止 PID 文件中且可执行路径匹配
的那一个 Caddy，不停止 DiPlay，也不会杀掉所有 Caddy 进程。重载脚本执行前验证配置。

卸载管理服务：先以 root 停止管理监督进程及其管理子进程，再移除其 service.d 文件。
不要删除 `/data/local/diplay-https`；现有网关和证书仍可独立工作。

## 验证边界

自动测试使用临时测试 CA / 假 ACME，不申请正式证书。生产条款查询是只读操作。
成功加载管理页不等于已验证一次真实续期；第一次真实操作应在到期前预留充足时间。
没有通过测试 CA 的证书不能安装；staging 配置永远禁止安装到生产网关。