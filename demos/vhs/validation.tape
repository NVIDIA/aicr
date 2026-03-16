Require aicr

Set Shell "bash"
Set FontSize 21
Set Width 1400
Set Height 800
Set WindowBar Colorful
Set Margin 10
Set MarginFill "#5c9300"
Set BorderRadius 5

Type "# post deployment cluster validation"
Enter
Type "aicr validate \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --phase deployment \"
Enter
Type "  --namespace aicr-validation \"
Enter
Type "  --toleration dedicated=worker-workload:NoSchedule \"
Enter
Type "  --toleration dedicated=worker-workload:NoExecute \"
Enter
Type "  --output deployment.json"
Sleep 1s
Enter
Sleep 55s
Ctrl+L


Type "# validation report in CTRF format"
Enter
Type "jq . deployment.json | bat -l json"
Enter
Sleep 2s
Ctrl+V
Sleep 2s
Ctrl+V
Sleep 2s
Ctrl+V
Sleep 2s
Ctrl+V
Sleep 2s
Ctrl+C
Ctrl+L

Type "# cluster performance validation"
Enter
Type "aicr validate \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --phase performance \"
Enter
Type "  --namespace aicr-validation \"
Enter
Type "  --toleration dedicated=worker-workload:NoSchedule \"
Enter
Type "  --toleration dedicated=worker-workload:NoExecute \"
Enter
Type "  --output performance.json"
Sleep 1s
Enter
Sleep 110s
Ctrl+L

Type "# performance report with NCCL bandwidth tests"
Enter
Type "jq .results.tests[0].stdout performance.json"
Enter
Sleep 3s
Ctrl+L

Type "# CNCF conformance validation"
Enter
Type "aicr validate \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --phase conformance \"
Enter
Type "  --namespace aicr-validation \"
Enter
Type "  --toleration dedicated=worker-workload:NoSchedule \"
Enter
Type "  --toleration dedicated=worker-workload:NoExecute \"
Enter
Type "  --output conformance.json"
Sleep 1s
Enter
Sleep 120s
Ctrl+L


Type "# validation report in CTRF format"
Enter
Type "jq 'del(.results.tests[].suite, .results.tests[].stdout, .results.tests[].duration)' conformance.json | bat -l json"
Enter
Sleep 2s
Ctrl+V
Sleep 2s
Ctrl+V
Sleep 2s
Ctrl+C
Ctrl+L

Type "# run all validations at once"
Enter
Type "aicr validate \"
Enter
Type "  --recipe recipe.yaml \"
Enter
Type "  --phase all \"
Enter
Type "  --output validation.json"
Sleep 1s
Enter
Ctrl+L
