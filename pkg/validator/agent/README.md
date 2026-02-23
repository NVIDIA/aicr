# Validation Agent

The validation agent package provides a Kubernetes Job-based executor for running validation checks.

## Architecture

The validation agent follows the same pattern as the snapshot agent (`pkg/k8s/agent`):

```
┌────────────────┐
│   Validator    │
│   (CLI/API)    │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ Agent Deployer │  ← Creates K8s resources
└───────┬────────┘
        │
        ├─► RBAC (ServiceAccount, Role, RoleBinding)
        ├─► Input ConfigMaps (snapshot.yaml, recipe.yaml)
        └─► Job (runs go test commands)
            │
            ├─► Mounts snapshot + recipe as volumes
            ├─► Runs: go test -json -run TestName
            └─► Writes results to ConfigMap
```

## Key Components

### Deployer
- `Deploy()` - Creates RBAC + Job
- `WaitForCompletion()` - Waits for Job to finish
- `GetResult()` - Retrieves validation results from ConfigMap
- `Cleanup()` - Removes Job and RBAC resources

### Job Execution
The Job container:
1. Mounts snapshot and recipe from ConfigMaps
2. Sets environment variables (AICR_SNAPSHOT_PATH, AICR_RECIPE_PATH)
3. Runs `go test -v -json <package>` (runs all tests in phase package)
4. Outputs test results to stdout (JSON format between markers)
5. Exits with test exit code

The validator reads Job logs and updates the unified ValidationResult ConfigMap.

### RBAC Permissions
The validation Job needs permissions to:
- Read/write ConfigMaps (for inputs and results)
- Read pods, services, deployments (for deployment phase checks)
- Read nodes (for deployment phase checks)

## Usage Example

```go
// Create Kubernetes client
clientset, err := k8sclient.GetKubeClient()

// Configure validation agent
config := agent.Config{
    Namespace:          "aicr-validation",
    JobName:            "aicr-validation-deployment",
    Image:              "ghcr.io/nvidia/aicr-validator:latest",  // Validator image with Go toolchain
    ServiceAccountName: "aicr-validator",
    SnapshotConfigMap:  "aicr-snapshot",
    RecipeConfigMap:    "aicr-recipe",
    TestPackage:        "./pkg/validator/checks/deployment",
    TestPattern:        "TestOperatorHealth",
    Timeout:            5 * time.Minute,
    Cleanup:            true,
}

// Create deployer
deployer := agent.NewDeployer(clientset, config)

// Deploy and wait
if err := deployer.Deploy(ctx); err != nil {
    return err
}

defer deployer.Cleanup(ctx, agent.CleanupOptions{Enabled: true})

if err := deployer.WaitForCompletion(ctx, config.Timeout); err != nil {
    return err
}

// Get results
result, err := deployer.GetResult(ctx)
if err != nil {
    return err
}

fmt.Printf("Check: %s, Status: %s\n", result.CheckName, result.Status)
```

## Design Decisions

### Why Jobs instead of inline execution?

1. **Isolation** - Tests run in cluster context with proper RBAC
2. **Resource limits** - Jobs can have CPU/memory constraints
3. **Node affinity** - Performance tests can target GPU nodes
4. **Observability** - Jobs show up in `kubectl get jobs`
5. **Reproducibility** - Same execution environment every time

### Why ConfigMaps for results?

1. **Persistence** - Results survive even if Job is deleted
2. **Accessibility** - Easy to retrieve with kubectl or API
3. **Size limits** - ConfigMap 1MB limit encourages concise results
4. **Standard pattern** - Consistent with snapshot agent

### Why one Job per phase?

1. **Early exit** - Can skip subsequent phases on failure
2. **Resource scheduling** - Different phases need different nodes
3. **Granular control** - Easier to retry specific phases
4. **Clear boundaries** - Each phase is independent unit of work

## Integration with Validator

The validator package uses this agent to run checks:

```go
// In pkg/validator/phases.go
func (v *Validator) validateDeployment(ctx context.Context, ...) {
    // For each check in recipe
    for _, checkName := range recipe.Validation.Deployment.Checks {
        // Deploy Job for this check
        deployer := agent.NewDeployer(...)
        deployer.Deploy(ctx)
        deployer.WaitForCompletion(ctx, timeout)
        result := deployer.GetResult(ctx)

        // Aggregate results
        phaseResult.Checks = append(phaseResult.Checks, result)
    }
}
```
