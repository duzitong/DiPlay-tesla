# USB 一键获取 Tesla 浏览器配对码

`Get-TeslaPairingCode.ps1` 使用 **PowerShell 7 + ADB** 操作 Android 设置界面。
无需 root、Python、scrcpy 或互联网。它不是手机远程取码服务，仍需带电脑通过 USB 连接 V50。

## 新版 0.1.11 独立测试包

在仓库根目录执行：

```powershell
pwsh -NoProfile -File .\scripts\Get-TeslaPairingCode.ps1
```

默认打开 `com.diplay.tesla.debug`。获取旧版正式包的码：

```powershell
pwsh -NoProfile -File .\scripts\Get-TeslaPairingCode.ps1 -App Release
```

多个 Android 设备连接时，添加 `-Serial 323952920752`；ADB 不在默认 SDK 路径或 PATH 时，
添加 `-AdbPath 'C:\你的目录\adb.exe'`。需要复制配对码时添加 `-Copy`，默认不修改剪贴板。

脚本唤醒设备，打开“设置 → 连接 → Tesla 浏览器”，在终端输出新的六位码。
如果配对窗口已经打开，会先关闭再打开，避免使用过期码。支持简体中文、繁体中文和英文界面。
页面布局/语言改变或系统权限弹窗可能中止自动导航，此时先在投屏里处理，再重新执行。
正常导航需要几次 UI 转储，通常几十秒；不会后台无限重试。

## 车上使用

1. 停车后用 USB 连接电脑与 V50，确保电脑已获得 ADB 授权。
2. 先确认 **目标 DiPlay 版本**已配置好连接并运行；不要同时运行原版和测试版。
3. 运行脚本，把码输入车机浏览器的 DiPlay 配对页面。有效期 5 分钟，输错 5 次须重新取码。
4. 使用稳定的浏览器入口（域名、协议、端口保持一致），正常情况下无需每次配对。
   更换入口、清除浏览器数据、更换 App 或设备会需要重新配对。

脚本不包含你设备的业务网址；它不会修改 DNAT、HTTPS 证书、热点、密码或应用设置，
不会断开 CarPlay、清数据、卸载或强停另一个版本。若选错版本且两版均在运行，端口可能不同。
脚本会启动 ADB 服务，但不启用 Windows USB 网卡；原有“USB 仅局域网”配置保持不变。

它只在终端显示短期配对码，不读浏览器长期令牌、Wi-Fi 密码或认证私钥。
临时 UI 文件在设备上使用随机名称，退出时删除；不要分享包含配对码的终端截图。

## 离线自检

```powershell
pwsh -NoProfile -File .\scripts\Get-TeslaPairingCode.ps1 -SelfTest
```

自检只用合成界面检查配对码解析和按钮匹配，不连接设备、不访问网络。