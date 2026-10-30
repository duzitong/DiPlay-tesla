#requires -Version 7.0
<#
.SYNOPSIS
Open DiPlay's Android UI over USB and show a newly generated browser pairing code.
.EXAMPLE
pwsh -File .\scripts\Get-TeslaPairingCode.ps1
.EXAMPLE
pwsh -File .\scripts\Get-TeslaPairingCode.ps1 -App Release -Copy
#>
[CmdletBinding()]
param(
    [ValidateSet('Debug', 'Release')][string]$App = 'Debug',
    [string]$Serial,
    [string]$AdbPath,
    [switch]$Copy,
    [switch]$SelfTest
)

$ErrorActionPreference = 'Stop'

function Get-PairingCode([xml]$Ui) {
    foreach ($node in $Ui.SelectNodes('//node')) {
        if ($node.GetAttribute('password') -eq 'true') { continue }
        $text = $node.GetAttribute('text')
        # Match the label, not arbitrary numbers in IPs, sizes or timestamps.
        $match = [regex]::Match($text, '(?i)(?:6\s*位配[对對]码|6\s*位配對碼|6[- ]digit pairing code|pairing code)\s*[:：]\s*(\d{6})(?!\d)')
        if ($match.Success) { return $match.Groups[1].Value }
    }
    return $null
}

function Test-Target($Node, [string]$Target) {
    if ($Node.GetAttribute('enabled') -ne 'true' -or $Node.GetAttribute('password') -eq 'true') { return $false }
    $text = $Node.GetAttribute('text')
    $description = $Node.GetAttribute('content-desc')
    switch ($Target) {
        'Settings' { return $text -cin @('设置', '設定', '設置', 'Settings') -and $Node.GetAttribute('class') -eq 'android.widget.Button' }
        'Connection' { return $description -match '^(打开连接设置|開啟連線設定|開啟連接設定|Open [Cc]onnection settings)$' }
        'Tesla' { return $text -match '^Tesla (浏览器|瀏覽器|browser).*\s[·・]\s' -and $Node.GetAttribute('class') -eq 'android.widget.Button' }
        'Return' { return $text -cin @('返回 DiPlay', 'Return to DiPlay', 'Back to DiPlay', '返回至 DiPlay') -and $Node.GetAttribute('class') -eq 'android.widget.Button' }
    }
    return $false
}

if ($SelfTest) {
    foreach ($label in @('6 位配对码', '6 位配對碼', '6-digit pairing code')) {
        $ui = [xml]("<hierarchy><node text='$label`:&amp;#10;123456' /></hierarchy>" -replace '&amp;#10;', '&#10;')
        if ((Get-PairingCode $ui) -ne '123456') { throw "Code parsing failed: $label" }
    }
    foreach ($text in @('http://192.168.0.1:8081', '123456', 'Pairing code: 1234567')) {
        if (Get-PairingCode ([xml]"<hierarchy><node text='$text' /></hierarchy>")) { throw 'Accepted a non-code' }
    }
    if (Get-PairingCode ([xml]"<hierarchy><node text='Pairing code: 123456' password='true' /></hierarchy>")) { throw 'Read a password field' }
    $sample = [xml]"<hierarchy><node text='断开连接' class='android.widget.Button' enabled='true' /><node text='设置' class='android.widget.Button' enabled='true' /><node text='Tesla 浏览器（实验性） · http://192.168.0.1:8081' class='android.widget.Button' enabled='true' /><node content-desc='Open connection settings' enabled='true' /></hierarchy>"
    if (Test-Target $sample.hierarchy.node[0] 'Settings') { throw 'Matched disconnect button' }
    if (!(Test-Target $sample.hierarchy.node[1] 'Settings')) { throw 'Settings matching failed' }
    if (!(Test-Target $sample.hierarchy.node[2] 'Tesla')) { throw 'Tesla matching failed' }
    if (!(Test-Target $sample.hierarchy.node[3] 'Connection')) { throw 'Connection matching failed' }
    Write-Output 'Pairing helper self-tests passed (no ADB or network access).'
    return
}

if (!$AdbPath) {
    $candidate = Join-Path $env:LOCALAPPDATA 'Android\Sdk\platform-tools\adb.exe'
    if (Test-Path -LiteralPath $candidate) { $AdbPath = $candidate }
    else {
        $command = Get-Command adb -ErrorAction SilentlyContinue
        if ($command) { $AdbPath = $command.Source }
    }
}
if (!$AdbPath -or !(Test-Path -LiteralPath $AdbPath)) { throw 'ADB not found. Supply -AdbPath with the path to adb.exe.' }
$script:AdbExecutable = (Resolve-Path -LiteralPath $AdbPath).Path
$script:DeviceSerial = ''

function Invoke-Adb([string[]]$AdbArgs) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $script:AdbExecutable
    $info.UseShellExecute = $false
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = [Text.UTF8Encoding]::new($false)
    $info.StandardErrorEncoding = [Text.UTF8Encoding]::new($false)
    if ($script:DeviceSerial) {
        $info.ArgumentList.Add('-s')
        $info.ArgumentList.Add($script:DeviceSerial)
    }
    foreach ($argument in $AdbArgs) { $info.ArgumentList.Add($argument) }
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $info
    try {
        [void]$process.Start()
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if (!$process.WaitForExit(45000)) {
            $process.Kill($true)
            throw 'ADB timed out. Check the USB cable and unlock/authorize the device.'
        }
        $output = $stdout.GetAwaiter().GetResult()
        [void]$stderr.GetAwaiter().GetResult()
        if ($process.ExitCode -ne 0) { throw 'ADB command failed. Check USB authorization and that DiPlay is installed.' }
        return $output.Trim()
    }
    finally { $process.Dispose() }
}

$devices = Invoke-Adb -AdbArgs @('devices')
$ready = @([regex]::Matches($devices, '(?m)^([^\s]+)\s+device\s*$') | ForEach-Object { $_.Groups[1].Value })
if ($Serial) {
    if ($Serial -notin $ready) { throw 'Selected device is not connected/authorized. Check adb devices.' }
    $script:DeviceSerial = $Serial
}
elseif ($ready.Count -eq 1) { $script:DeviceSerial = $ready[0] }
else { throw 'Connect one authorized device, or select it with -Serial. Check adb devices.' }

$package = if ($App -eq 'Debug') { 'com.diplay.tesla.debug' } else { 'com.diplay.tesla' }
if ((Invoke-Adb -AdbArgs @('shell', 'pm', 'path', $package)) -notmatch '(?m)^package:') {
    throw "App not installed: $package. Use -App Release for the original app."
}
$script:DumpPath = '/data/local/tmp/diplay-pair-' + [guid]::NewGuid().ToString('N') + '.xml'
$script:Deadline = [datetime]::UtcNow.AddMinutes(4)

function Read-Ui {
    if ([datetime]::UtcNow -gt $script:Deadline) { throw 'UI navigation timed out. Open Settings > Connection manually, then rerun.' }
    # Delete first: never accept a stale dump from a sleeping/locked screen.
    [void](Invoke-Adb -AdbArgs @('shell', 'rm', '-f', $script:DumpPath))
    [void](Invoke-Adb -AdbArgs @('shell', 'uiautomator', 'dump', $script:DumpPath))
    $xmlText = Invoke-Adb -AdbArgs @('shell', 'cat', $script:DumpPath)
    $ui = [xml]::new()
    $ui.XmlResolver = $null
    $ui.LoadXml($xmlText)
    $packages = @($ui.SelectNodes('//node') | ForEach-Object { $_.GetAttribute('package') } | Sort-Object -Unique)
    if ($package -notin $packages) { throw 'DiPlay is not visible. Unlock the device or dismiss its system permission dialog, then rerun.' }
    return ,$ui
}

function Open-Target([string]$Target, [xml]$InitialUi) {
    $ui = $InitialUi
    # Only Enter on a positively identified focused control. Tab can scroll an
    # off-screen Settings button into focus on the headless V50.
    for ($i = 0; $i -lt 24; $i++) {
        $focused = $ui.SelectSingleNode('//node[@focused="true"]')
        if ($focused -and (Test-Target $focused $Target)) {
            [void](Invoke-Adb -AdbArgs @('shell', 'input', 'keyevent', 'KEYCODE_ENTER'))
            return ,(Read-Ui)
        }
        [void](Invoke-Adb -AdbArgs @('shell', 'input', 'keyevent', 'KEYCODE_TAB'))
        $ui = Read-Ui
    }
    throw "Cannot find $Target. Supported UI languages: Simplified/Traditional Chinese and English. Open Settings > Connection > Tesla browser manually."
}

try {
    Write-Host "Opening $package on $($script:DeviceSerial)..."
    [void](Invoke-Adb -AdbArgs @('shell', 'input', 'keyevent', 'KEYCODE_WAKEUP'))
    $component = "$package/com.shilapi.xcertplay.DiPlayActivity"
    [void](Invoke-Adb -AdbArgs @('shell', 'am', 'start', '-n', $component))
    $ui = Read-Ui
    if (Get-PairingCode $ui) {
        # Close only a positively identified pairing dialog, then reopen for a
        # fresh five-minute code. Never return an old dialog's expired value.
        [void](Invoke-Adb -AdbArgs @('shell', 'input', 'keyevent', 'KEYCODE_BACK'))
        $ui = Read-Ui
    }
    if (@($ui.SelectNodes('//node') | Where-Object { Test-Target $_ 'Return' }).Count) {
        $ui = Open-Target 'Return' $ui
    }
    if (@($ui.SelectNodes('//node') | Where-Object { Test-Target $_ 'Tesla' }).Count) {
        $ui = Open-Target 'Tesla' $ui
    }
    else {
        if (!@($ui.SelectNodes('//node') | Where-Object { Test-Target $_ 'Connection' }).Count) {
            $ui = Open-Target 'Settings' $ui
        }
        $ui = Open-Target 'Connection' $ui
        $ui = Open-Target 'Tesla' $ui
    }
    $code = Get-PairingCode $ui
    if (!$code) { throw 'Pairing window opened, but no six-digit code was found. Check the device preview.' }
    Write-Host ''
    Write-Host "Pairing code: $code   (valid for 5 minutes)" -ForegroundColor Green
    Write-Host 'Enter it in the target browser. Keep that browser origin unchanged to retain pairing.'
    if ($Copy) { Set-Clipboard -Value $code; Write-Host 'Code copied to clipboard.' }
}
finally {
    # Never leave a pairing-code UI dump on the device. No screenshots/log files.
    try { [void](Invoke-Adb -AdbArgs @('shell', 'rm', '-f', $script:DumpPath)) } catch { Write-Warning 'Could not remove temporary UI dump after USB disconnect.' }
}