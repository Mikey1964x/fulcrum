<#
.SYNOPSIS
Builds a release version of fulcrum.

.DESCRIPTION
Builds the same way the CI release job does: a static binary without debug
information, stamped with its version. By default it builds for this machine
into bin\.

.PARAMETER All
Build the three release targets (windows/amd64, linux/amd64, darwin/arm64)
into dist\, with a SHA256SUMS file, instead of this machine's into bin\.

.PARAMETER Version
Version reported by "fulcrum -version". Default: git describe, such as
v0.3.2 or v0.3.2-3-g1a2b3c4-dirty.

.EXAMPLE
scripts\build.ps1

.EXAMPLE
scripts\build.ps1 -All -Version v0.4.0
#>
[CmdletBinding()]
param(
    [switch]$All,
    [string]$Version
)

$ErrorActionPreference = 'Stop'

# Run from the repository root, wherever the script is called from.
Push-Location (Join-Path $PSScriptRoot '..')
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    if (-not $Version) {
        $Version = git describe --tags --always --dirty 2>$null
        if (-not $Version) { $Version = 'dev' }
    }

    function Build([string]$GoOS, [string]$GoArch, [string]$Output) {
        Write-Host "building $Output ($GoOS/$GoArch, $Version)"
        $env:GOOS = $GoOS
        $env:GOARCH = $GoArch
        $env:CGO_ENABLED = '0'
        go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $Output ./src/cmd/fulcrum
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $GoOS/$GoArch" }
    }

    if ($All) {
        if (Test-Path dist) { Remove-Item -Recurse -Force dist }
        New-Item -ItemType Directory dist | Out-Null
        Build windows amd64 dist/fulcrum-windows-amd64.exe
        Build linux amd64 dist/fulcrum-linux-amd64
        Build darwin arm64 dist/fulcrum-darwin-arm64
        # Same format as sha256sum, with LF line endings, so the file can be
        # checked with "sha256sum -c".
        $lines = Get-ChildItem dist -Filter 'fulcrum-*' | Sort-Object Name | ForEach-Object {
            '{0}  {1}' -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name
        }
        [IO.File]::WriteAllText((Join-Path (Get-Location) 'dist/SHA256SUMS'), (($lines -join "`n") + "`n"))
        Write-Host 'wrote dist/SHA256SUMS'
    }
    else {
        # Ask Go for this machine's target with any GOOS/GOARCH override
        # cleared, so a value left in the environment does not redirect it.
        $env:GOOS = $null
        $env:GOARCH = $null
        $goos = go env GOOS
        $goarch = go env GOARCH
        $ext = if ($goos -eq 'windows') { '.exe' } else { '' }
        Build $goos $goarch "bin/fulcrum$ext"
    }
}
finally {
    foreach ($name in $saved.Keys) { Set-Item "env:$name" $saved[$name] }
    Pop-Location
}
