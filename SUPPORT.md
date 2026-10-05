# Support

Where to go depends on what you need.

| Need | Where |
|------|-------|
| A question, or help using AICR | `#aicr` on Kubernetes Slack: [get an invite](https://slack.k8s.io/), then [open the channel](https://kubernetes.slack.com/archives/C0AQMPP1BK7) |
| A bug or a scoped feature request | [GitHub Issues](https://github.com/NVIDIA/aicr/issues/new/choose) |
| A security vulnerability | Follow [SECURITY.md](SECURITY.md); do not report it through GitHub |

## Before You Ask

- Check the [documentation](https://docs.nvidia.com/aicr), including the troubleshooting section of the relevant guide.
- Search [existing issues](https://github.com/NVIDIA/aicr/issues?q=is%3Aissue) for the same problem.

## What to Include

Redact credentials, tokens, and other sensitive values from everything you share. Commands, their output, recipes, and snapshots can all contain them. For example, a command may pass a registry password as an environment variable.

- The `aicr --version` output.
- The command you ran and its full output.
- Your recipe criteria (service, accelerator, OS, intent, platform), or the recipe file itself.
- A cluster snapshot (`aicr snapshot`) when the problem depends on cluster state.

Contributing code or docs? See [CONTRIBUTING.md](CONTRIBUTING.md).
