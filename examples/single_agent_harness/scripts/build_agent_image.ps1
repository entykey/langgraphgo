$ProjectDir = Split-Path -Parent $PSScriptRoot

docker build -f "$ProjectDir\Dockerfile.code-exec" -t mas-agent-exec:latest $ProjectDir
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "Built mas-agent-exec:latest"

docker run --rm mas-agent-exec:latest python3 -c `
    "import pandas, openpyxl, matplotlib, numpy, docx, pdfminer, PIL, requests; print('Import check: OK')"
