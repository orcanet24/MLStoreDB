# apply_comment_patch.ps1
# Aplica parches de traduccion de comentarios SIN tocar el codigo.
#
# Formato del archivo de parche (.patch):
#   ===FILE db/index.go            -> fija el archivo destino (ruta relativa a la raiz del repo)
#   --- 14 14                      -> reemplaza las lineas 14..14 originales (1-based, inclusive)
#   // texto en espanol            -> contenido nuevo (una o mas lineas, con tabulaciones reales)
#   --- 20 21
#   // mas texto
#   // mas texto
#
# Reglas:
#  * Las lineas de contenido son literales (se conservan tabulaciones y espacios).
#  * Los rangos se refieren SIEMPRE al archivo ORIGINAL (no se acumulan desplazamientos).
#  * Los rangos de un mismo archivo no deben solaparse.
#  * Se preserva el terminador de linea original (CRLF o LF) y la codificacion UTF-8 sin BOM.
#
# Uso:
#   pwsh -File scripts/apply_comment_patch.ps1 -PatchDir scripts/patches
#   pwsh -File scripts/apply_comment_patch.ps1 -PatchDir scripts/patches -DryRun

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PatchDir,
    [switch]$DryRun,
    [switch]$Gofmt,
    [string]$Only = '*'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

function Get-PatchOps {
    param([string]$Path)
    $ops = New-Object System.Collections.Generic.List[object]
    $currentFile = $null
    $start = -1; $end = -1
    $buf = New-Object System.Collections.Generic.List[string]
    $raw = [System.IO.File]::ReadAllText($Path)
    $lines = $raw -split "`r?`n"
    # Quita el elemento vacio final que produce -split cuando el archivo acaba en EOL.
    if ($lines.Count -gt 0 -and $lines[$lines.Count - 1] -eq '') { $lines = $lines[0..($lines.Count - 2)] }

    foreach ($line in $lines) {
        if ($line -match '^===FILE\s+(.+?)\s*$') {
            if ($start -ge 0) {
                $ops.Add([pscustomobject]@{ File = $currentFile; Start = $start; End = $end; Text = ($buf -join "`n") })
                $start = -1; $buf.Clear()
            }
            $currentFile = $Matches[1]
            continue
        }
        if ($line -match '^===') {
            if ($start -ge 0) {
                $ops.Add([pscustomobject]@{ File = $currentFile; Start = $start; End = $end; Text = ($buf -join "`n") })
                $start = -1; $buf.Clear()
            }
            continue
        }
        if ($line -match '^---\s+(\d+)\s+(\d+)\s*$') {
            if ($start -ge 0) {
                $ops.Add([pscustomobject]@{ File = $currentFile; Start = $start; End = $end; Text = ($buf -join "`n") })
                $buf.Clear()
            }
            $start = [int]$Matches[1]
            $end = [int]$Matches[2]
            continue
        }
        if ($start -ge 0) { $buf.Add($line) }
    }
    if ($start -ge 0) {
        $ops.Add([pscustomobject]@{ File = $currentFile; Start = $start; End = $end; Text = ($buf -join "`n") })
    }
    return $ops
}

$patchFiles = Get-ChildItem -Path $PatchDir -Filter *.patch -File -Recurse | Where-Object { $_.Name -like $Only }
if (-not $patchFiles) { throw "No se encontraron archivos .patch en $PatchDir" }

$allOps = @{}
foreach ($pf in $patchFiles) {
    $ops = Get-PatchOps -Path $pf.FullName
    foreach ($op in $ops) {
        if (-not $allOps.ContainsKey($op.File)) { $allOps[$op.File] = New-Object System.Collections.Generic.List[object] }
        $allOps[$op.File].Add($op)
    }
}

$totalOps = 0
foreach ($file in ($allOps.Keys | Sort-Object)) {
    $full = Join-Path $repoRoot $file
    if (-not (Test-Path $full)) { throw "No existe: $full" }
    $raw = [System.IO.File]::ReadAllText($full)
    $eol = "`n"
    if ($raw.Contains("`r`n")) { $eol = "`r`n" }
    $orig = $raw -split "`r?`n"
    $ops = $allOps[$file] | Sort-Object Start

    $prevEnd = 0
    foreach ($op in $ops) {
        if ($op.Start -lt 1 -or $op.End -gt $orig.Count) {
            throw "$file : rango fuera de limites ($($op.Start)-$($op.End)) sobre $($orig.Count) lineas"
        }
        if ($op.Start -le $prevEnd) { throw "$file : rangos solapados en la linea $($op.Start)" }
        $prevEnd = $op.End
    }

    $out = New-Object System.Collections.Generic.List[string]
    $cursor = 0 # indice 0-based de la siguiente linea original a copiar
    $nOps = @($ops).Count
    foreach ($op in $ops) {
        $s = $op.Start - 1
        # Si TODAS las lineas originales del rango son lineas de comentario completas
        # (o estan en blanco), se reutiliza su indentacion original: en el parche el
        # texto se escribe pegado a la izquierda (sin tabulaciones) y aqui se le
        # vuelve a poner la sangria. Las lineas que ya empiezan con espacios/tab se
        # respetan tal cual (util para listas con sangria propia).
        $allComment = $true
        $indent = ''
        $from = $op.Start - 1
        $to = [Math]::Min($op.End - 1, $orig.Count - 1)
        for ($k = $from; $k -le $to; $k++) {
            $t = $orig[$k].Trim()
            if ($t -eq '') { continue }
            if ($t -notmatch '^//') { $allComment = $false; break }
            if ($indent -eq '') { $indent = [regex]::Match($orig[$k], '^\s*').Value }
        }
        while ($cursor -lt $s) { $out.Add($orig[$cursor]); $cursor++ }
        if ($op.Text.Length -gt 0) {
            foreach ($l in ($op.Text -split "`n")) {
                if ($l -eq '') { $out.Add('') }
                elseif ($allComment -and $l -notmatch '^\s') { $out.Add($indent + $l) }
                else { $out.Add($l) }
            }
        }
        $cursor = $op.End
        $totalOps++
    }
    while ($cursor -lt $orig.Count) { $out.Add($orig[$cursor]); $cursor++ }

    # La division y reunion agrega un elemento vacio al final si el archivo terminaba con EOL.
    $text = ($out -join $eol)
    if (-not $DryRun) {
        [System.IO.File]::WriteAllText($full, $text, (New-Object System.Text.UTF8Encoding($false)))
        if ($Gofmt) { & gofmt -w $full }
    }
    Write-Host ("{0,-45} {1,4} parches" -f $file, $nOps)
}

# Marca los parches como aplicados (evita re-aplicarlos por error: los rangos
# se refieren a las lineas originales y ya no serian validos).
if (-not $DryRun) {
    foreach ($pf in $patchFiles) {
        Rename-Item -Path $pf.FullName -NewName ($pf.Name + '.applied')
    }
}

Write-Host ("Total: {0} parches en {1} archivos{2}" -f $totalOps, $allOps.Count, $(if ($DryRun) { ' (DRY RUN)' } else { '' }))
