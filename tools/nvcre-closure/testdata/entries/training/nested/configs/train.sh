#!/bin/bash
set -euo pipefail
repo="{{ if .SourceRepo }}{{ .SourceRepo }}{{ else }}https://example.invalid/Trainer.git{{ end }}"
git clone --depth 1 -b v1.0.0 "$repo" /workspace/trainer
{{ lib "scripts/fetch-data.sh" . }}
