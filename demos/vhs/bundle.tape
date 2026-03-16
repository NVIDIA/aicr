Require aicr

Set Shell "bash"
Set FontSize 21
Set Width 1400
Set Height 800
Set WindowBar Colorful
Set Margin 10
Set MarginFill "#5c9300"
Set BorderRadius 5

Type "# simple bundle"
Enter
Type "aicr bundle --recipe recipe.yaml --output ./bundle"
Sleep 1s
Enter
Sleep 3s
Ctrl+L

Type "# by default, bundle follows the standard Helm folder per component structure"
Enter
Type "tree -L 1 --dirsfirst -F -C ./bundle"
Enter
Sleep 3s
Ctrl+L

Type "# each component has its own readme and Helm chart values or manifest files"
Enter
Type "tree --dirsfirst -F -C bundle/nvidia-dra-driver-gpu"
Enter
Sleep 3s
Ctrl+L

Type "# more advanced bundle"
Enter
Type "aicr bundle \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --accelerated-node-selector nodeGroup=gpu-worker \"
Enter
Type "  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \"
Enter
Type "  --system-node-selector dedicated=system-workload  \"
Enter
Type "  --set gpuoperator:driver.version=570.86.16 \"
Enter
Type "  --nodes 250 \"
Enter
Type "  --output ./bundle"
Sleep 2s
Enter
Sleep 2s
Ctrl+L

Type "# deployer specific bundle"
Enter
Type "aicr bundle \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --deployer argocd \"
Enter
Type "  --repo https://github.com/my-org/my-gitops-repo.git \"
Enter
Type "  --output ./bundle"
Sleep 1s
Enter
Sleep 2s
Ctrl+L

Type "# ArgoCD app of apps"
Enter
Type "yq . bundle/app-of-apps.yaml | bat -l yaml"
Enter
Sleep 3s
Ctrl+C
Ctrl+L

Type "# bundle as OCI image"
Enter
Type "aicr bundle \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --deployer argocd \"
Enter
Type "  --output oci://ghcr.io/nvidia/aicr-bundle "
Enter
Sleep 8s
Ctrl+L

Type "# cosign attested bundle"
Enter
Type "aicr bundle --recipe recipe.yaml --output ./bundle --attest"
Enter
Sleep 8s
Ctrl+L

Type "# attestations for both aicr and the generated bundle"
Enter
Type "tree -C ./bundle/attestation"
Enter
Sleep 4s
Ctrl+L

Type "# cryptographic signature verified against Sigstore trusted root"
Enter
Type "aicr verify ./bundle"
Enter
Sleep 5s
