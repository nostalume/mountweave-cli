# Installs one verified mweave release for the current user.
[CmdletBinding()]
param(
    [string] $Version,
    [string] $BinDir
)

$ErrorActionPreference = 'Stop'
$repo = 'https://github.com/nostalume/mountweave-cli'
$architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($architecture) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw 'Unsupported Windows CPU architecture.' }
}
if (!$PSBoundParameters.ContainsKey('BinDir')) {
    if (!$env:LOCALAPPDATA) { throw 'Set LOCALAPPDATA or provide -BinDir.' }
    $BinDir = Join-Path $env:LOCALAPPDATA 'Programs\mweave'
}
if (!$BinDir) { throw '-BinDir needs a directory.' }
$BinDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($BinDir)

if (!$PSBoundParameters.ContainsKey('Version')) {
    $latest = Invoke-RestMethod -Uri 'https://api.github.com/repos/nostalume/mountweave-cli/releases/latest'
    $Version = $latest.tag_name
}
if ($Version -cnotmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?\z') {
    throw 'Expected a release tag such as v0.1.0.'
}

$asset = "mweave_$($Version)_windows_$arch.zip"
$base = "$repo/releases/download/$Version"
$tempDir = Join-Path ([IO.Path]::GetTempPath()) ('mweave-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($tempDir)
$checksum = Join-Path $tempDir 'SHA256SUMS'
$archive = Join-Path $tempDir $asset
$staged = $null
try {
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $checksum
    $pattern = '^([0-9a-fA-F]{64})[ \t]+' + [regex]::Escape($asset) + '$'
    $checksumEntries = @(Select-String -LiteralPath $checksum -Pattern $pattern -CaseSensitive)
    if ($checksumEntries.Count -ne 1) { throw "No unique SHA-256 checksum for $asset." }
    $expected = $checksumEntries[0].Matches[0].Groups[1].Value
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive
    if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $expected) {
        throw "SHA-256 mismatch for $asset."
    }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($archive)
    try {
        $entry = $zip.GetEntry('mweave.exe')
        if (!$entry -or $entry.Length -eq 0) { throw 'The archive does not contain mweave.exe.' }
        [void][IO.Directory]::CreateDirectory($BinDir)
        $staged = Join-Path $BinDir ('.mweave-' + [guid]::NewGuid().ToString('N') + '.exe')
        $output = [IO.File]::Open($staged, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
        try {
            $inputStream = $entry.Open()
            try { $inputStream.CopyTo($output) } finally { $inputStream.Dispose() }
        } finally {
            $output.Dispose()
        }
    } finally {
        $zip.Dispose()
    }

    $target = Join-Path $BinDir 'mweave.exe'
    try {
        if ([IO.File]::Exists($target)) {
            [IO.File]::Replace($staged, $target, [NullString]::Value)
        } else {
            [IO.File]::Move($staged, $target)
        }
    } catch [IO.IOException] {
        throw "Could not replace $target. Close any running mweave process and retry. $($_.Exception.Message)"
    }
    $staged = $null
} finally {
    if ($staged) { [IO.File]::Delete($staged) }
    [IO.File]::Delete($archive)
    [IO.File]::Delete($checksum)
    [IO.Directory]::Delete($tempDir)
}

Write-Output "Installed mweave $Version at $target"
if (($env:Path -split [IO.Path]::PathSeparator) -notcontains $BinDir) {
    Write-Output "Add $BinDir to PATH to use the mweave command."
}
