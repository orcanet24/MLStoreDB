# verify_code_intact.ps1
# Comprueba que solo se han modificado COMENTARIOS: quita los comentarios de
# linea (//...) de cada archivo actual y de su copia de seguridad, y compara el
# codigo resultante linea a linea (posicional).
#
# Uso:  pwsh -File scripts/verify_code_intact.ps1 -BackupDir "$env:TEMP\mldb_orig"

[CmdletBinding()]
param([Parameter(Mandatory = $true)][string]$BackupDir)

$repoRoot = Split-Path -Parent $PSScriptRoot

function Get-CodeText {
    param([string]$Path)
    $lines = foreach ($l in (Get-Content -LiteralPath $Path)) {
        $i = $l.IndexOf('//')
        if ($i -ge 0) { $l.Substring(0, $i) } else { $l }
    }
    return (@($lines | ForEach-Object { $_.TrimEnd() } | Where-Object { $_ -ne '' }) -join "`n")
}

$checked = 0; $bad = 0
Get-ChildItem -Path $repoRoot -Recurse -File -Include *.go, *.ps1 |
    Where-Object { $_.FullName -notmatch '\\\.git\\' -and $_.FullName -notmatch '\\scripts\\' } |
    ForEach-Object {
        $rel = $_.FullName.Substring($repoRoot.Length + 1)
        $orig = Join-Path $BackupDir $rel
        if (-not (Test-Path -LiteralPath $orig)) { return }
        if ((Get-CodeText $orig) -ne (Get-CodeText $_.FullName)) {
            Write-Host "CODIGO DIFERENTE: $rel"
            $bad++
        }
        $checked++
    }

Write-Host ("Verificados {0} archivos; {1} con diferencias de codigo." -f $checked, $bad)
