param(
    [Parameter(Mandatory = $true)]
    [string[]] $Binaries
)

$ErrorActionPreference = 'Stop'
$vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
$dumpbin = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -find 'VC\Tools\MSVC\**\bin\Hostx64\x64\dumpbin.exe' | Select-Object -First 1
if (-not $dumpbin) { throw 'MSVC dumpbin.exe not found' }

foreach ($binary in $Binaries) {
    $dependencies = & $dumpbin /nologo /dependents $binary
    if ($LASTEXITCODE -ne 0) { throw "Cannot inspect dependencies: $binary" }
    $dependencies | Write-Output
    # UCRT is part of supported Windows; these VC++ runtime DLLs are not.
    $runtime = $dependencies | Where-Object {
        $_ -match '^\s*(?:vcruntime\d[^\s]*|msvcp\d[^\s]*|msvcr\d[^\s]*|concrt\d[^\s]*|vccorlib\d[^\s]*|ucrtbased)\.dll\s*$'
    }
    if ($runtime) {
        throw "$binary requires the Visual C++ Redistributable: $($runtime.Trim() -join ', ')"
    }
}
