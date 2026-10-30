<#
发版前置检查（本地执行，CI 不做这些）。

流水线只负责编译与发布，所有质量门槛都在这里：
格式、vet、测试、race、四平台编译、版本/文档一致性。

用法：
    pwsh ./scripts/preflight.ps1                 # 检查工作区版本
    pwsh ./scripts/preflight.ps1 -Tag 0.10        # 额外校验准备打的 tag 与版本一致
    pwsh ./scripts/preflight.ps1 -SkipRace        # 没有 gcc、跑不了 race 时
#>
[CmdletBinding()]
param(
    [string]$Tag,
    [switch]$SkipRace
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$failed = [System.Collections.Generic.List[string]]::new()

function Step {
    param([string]$Name, [scriptblock]$Body)
    Write-Host "== $Name" -ForegroundColor Cyan
    try {
        & $Body
        Write-Host "   ok" -ForegroundColor Green
    } catch {
        Write-Host "   FAILED: $_" -ForegroundColor Red
        $script:failed.Add($Name)
    }
}

# 版本号是发版的唯一事实来源，tag 必须与它一致。
$versionLine = Select-String -Path main.go -Pattern '^const version = "(.*)"$' |
    Select-Object -First 1
if (-not $versionLine) {
    throw 'main.go 里找不到 const version'
}
$version = $versionLine.Matches[0].Groups[1].Value
Write-Host "版本: $version" -ForegroundColor Yellow
if ($Tag) {
    Write-Host "目标 tag: $Tag" -ForegroundColor Yellow
}

Step '版本与 tag 一致' {
    if (-not $Tag) { return }
    if ($Tag -ne $version) {
        throw "tag '$Tag' 与 main.go 的版本 '$version' 不一致"
    }
}

Step '工作区没有未提交改动' {
    $dirty = git status --porcelain
    if ($dirty) {
        throw "存在未提交改动，先提交再发版：`n$dirty"
    }
}

Step 'gofmt 无待格式化文件' {
    $files = Get-ChildItem -Recurse -Filter *.go |
        Where-Object { $_.FullName -notmatch '[\\/]\.git[\\/]' } |
        ForEach-Object { gofmt -l $_.FullName }
    if ($files) {
        throw "以下文件需要 gofmt：`n$($files -join "`n")"
    }
}

Step 'go vet' {
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet 失败' }
}

Step 'go test' {
    go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw 'go test 失败' }
}

if (-not $SkipRace) {
    Step 'go test -race' {
        $env:CGO_ENABLED = '1'
        try {
            go test -race -count=1 ./...
            if ($LASTEXITCODE -ne 0) { throw 'go test -race 失败' }
        } finally {
            $env:CGO_ENABLED = '0'
        }
    }
}

Step '四平台交叉编译' {
    $out = Join-Path $env:TEMP "wb-preflight-$version"
    New-Item -ItemType Directory -Path $out -Force | Out-Null
    $targets = @(
        @('linux', 'amd64', 'webBenchmark_linux_x64'),
        @('linux', 'arm', 'webBenchmark_linux_arm'),
        @('windows', 'amd64', 'webBenchmark_x64.exe'),
        @('freebsd', 'amd64', 'webBenchmark_bsd_x64.exe')
    )
    $env:CGO_ENABLED = '0'
    foreach ($t in $targets) {
        $env:GOOS = $t[0]; $env:GOARCH = $t[1]
        go build -trimpath -o (Join-Path $out $t[2]) -buildvcs=false .
        if ($LASTEXITCODE -ne 0) { throw "$($t[0])/$($t[1]) 编译失败" }
    }
    Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    Write-Host "   产物在 $out" -ForegroundColor DarkGray
}

Step 'CHANGELOG 有对应版本小节' {
    $pattern = "^## $([regex]::Escape($version))$"
    if (-not (Select-String -Path CHANGELOG.md -Pattern $pattern)) {
        throw "CHANGELOG.md 缺少 '## $version' 小节"
    }
}

Step 'README 版本号与代码一致' {
    if (-not (Select-String -Path README.md -Pattern "Current version:\s*$([regex]::Escape($version))")) {
        throw "README.md 里的 'Current version' 不是 $version"
    }
}

# 参数表最容易和代码漂移：新增开关忘了写文档，这里直接拦下来。
Step 'README 覆盖了所有命令行开关' {
    $source = Get-Content main.go -Raw
    $names = [System.Collections.Generic.List[string]]::new()
    # 类型必须覆盖全部 flag.X(...)：漏掉 Float64 之类会让新开关静默逃过这道门禁。
    foreach ($m in [regex]::Matches($source, 'flag\.(?:Bool|Int|Int64|Uint|String|Float64|Duration)\("([^"]+)"')) {
        $names.Add($m.Groups[1].Value)
    }
    foreach ($m in [regex]::Matches($source, 'flag\.Var\(&\w+,\s*"([^"]+)"')) {
        $names.Add($m.Groups[1].Value)
    }

    $readme = Get-Content README.md -Raw
    # -h 是帮助开关，约定不写进参数表。
    $allowed = @('h')
    $missing = @($names | Sort-Object -Unique | Where-Object {
        $n = $_
        ($allowed -notcontains $n) -and ($readme -notmatch "(?m)^\s+-$([regex]::Escape($n))\b")
    })
    if ($missing) {
        throw "README 参数表缺少：$($missing -join ', ')"
    }
}

Write-Host ''
if ($failed.Count -gt 0) {
    Write-Host "前置检查未通过（$($failed.Count) 项）：" -ForegroundColor Red
    $failed | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
    exit 1
}

Write-Host "前置检查全部通过，可以打 tag 发版：" -ForegroundColor Green
Write-Host "  git tag $version && git push origin $version"
