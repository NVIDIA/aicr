/**
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

export default withMermaid(
  defineConfig({
    title: 'NVIDIA AI Cluster Runtime',
    description: 'Optimized, validated, and reproducible Kubernetes configurations for GPU infrastructure',
    base: '/',
    appearance: 'dark',
    lastUpdated: true,
    ignoreDeadLinks: [
      /localhost/,  // Example URLs in docs
      /\/index$/,   // VitePress resolves these automatically
      /github\.com\/NVIDIA\/aicr\/(blob|tree)\/main\//,  // GitHub source links (valid but not local)
    ],

    markdown: {
      // Prevent Vue from interpreting {{ }} in code blocks as template syntax
      // (common in Go templates, Helm, GitHub Actions, Ansible)
      attrs: { disable: true },
    },

    vue: {
      template: {
        compilerOptions: {
          // Treat all {{ }} outside of Vue components as raw text
        },
      },
    },

    head: [
      ['link', { rel: 'icon', type: 'image/png', href: '/images/favicon.png' }],
    ],

    themeConfig: {
      logo: undefined,
      siteTitle: 'AI Cluster Runtime',

      nav: [
        { text: 'Docs', link: '/docs/' },
        { text: 'GitHub', link: 'https://github.com/NVIDIA/aicr' },
      ],

      sidebar: {
        '/docs/': [
          {
            text: 'Getting Started',
            link: '/docs/getting-started/',
          },
          {
            text: 'User Guide',
            collapsed: false,
            items: [
              { text: 'Installation', link: '/docs/user/installation' },
              { text: 'CLI Reference', link: '/docs/user/cli-reference' },
              { text: 'API Reference', link: '/docs/user/api-reference' },
              { text: 'Agent Deployment', link: '/docs/user/agent-deployment' },
              { text: 'Component Catalog', link: '/docs/user/component-catalog' },
            ],
          },
          {
            text: 'Integrator Guide',
            collapsed: false,
            items: [
              { text: 'Automation', link: '/docs/integrator/automation' },
              { text: 'Data Flow', link: '/docs/integrator/data-flow' },
              { text: 'Kubernetes Deployment', link: '/docs/integrator/kubernetes-deployment' },
              { text: 'EKS Dynamo Networking', link: '/docs/integrator/eks-dynamo-networking' },
              { text: 'Recipe Development', link: '/docs/integrator/recipe-development' },
              { text: 'Validator Extension', link: '/docs/integrator/validator-extension' },
            ],
          },
          {
            text: 'Contributor Guide',
            collapsed: false,
            items: [
              { text: 'CLI', link: '/docs/contributor/cli' },
              { text: 'API Server', link: '/docs/contributor/api-server' },
              { text: 'Data Architecture', link: '/docs/contributor/data' },
              { text: 'Component Development', link: '/docs/contributor/component' },
              { text: 'Validations', link: '/docs/contributor/validations' },
              { text: 'Validator Development', link: '/docs/contributor/validator' },
            ],
          },
          {
            text: 'Conformance',
            collapsed: true,
            items: [
              { text: 'Overview', link: '/docs/conformance/' },
              { text: 'DRA Support', link: '/docs/conformance/evidence/dra-support' },
              { text: 'Gang Scheduling', link: '/docs/conformance/evidence/gang-scheduling' },
              { text: 'Secure Accelerator Access', link: '/docs/conformance/evidence/secure-accelerator-access' },
              { text: 'Accelerator Metrics', link: '/docs/conformance/evidence/accelerator-metrics' },
              { text: 'Inference Gateway', link: '/docs/conformance/evidence/inference-gateway' },
              { text: 'Robust Operator', link: '/docs/conformance/evidence/robust-operator' },
              { text: 'Pod Autoscaling', link: '/docs/conformance/evidence/pod-autoscaling' },
              { text: 'Cluster Autoscaling', link: '/docs/conformance/evidence/cluster-autoscaling' },
            ],
          },
          {
            text: 'Project',
            collapsed: true,
            items: [
              { text: 'Contributing', link: '/docs/project/contributing' },
              { text: 'Development', link: '/docs/project/development' },
              { text: 'Releasing', link: '/docs/project/releasing' },
              { text: 'Changelog', link: '/docs/project/changelog' },
              { text: 'Roadmap', link: '/docs/project/roadmap' },
              { text: 'Security', link: '/docs/project/security' },
              { text: 'Code of Conduct', link: '/docs/project/code-of-conduct' },
              { text: 'Maintainers', link: '/docs/project/maintainers' },
            ],
          },
        ],
      },

      socialLinks: [
        { icon: 'github', link: 'https://github.com/NVIDIA/aicr' },
      ],

      search: {
        provider: 'local',
      },

      editLink: {
        // Synced docs come from docs/ in the repo, not site/docs/
        pattern: ({ filePath }) => {
          const synced = filePath.match(/^docs\/(user|integrator|contributor)\/(.+)/)
          if (synced) {
            return `https://github.com/NVIDIA/aicr/edit/main/docs/${synced[1]}/${synced[2]}`
          }
          const conformanceEvidence = filePath.match(/^docs\/conformance\/evidence\/(.+)/)
          if (conformanceEvidence) {
            return `https://github.com/NVIDIA/aicr/edit/main/docs/conformance/cncf/evidence/${conformanceEvidence[1]}`
          }
          if (filePath === 'docs/conformance/index.md') {
            return 'https://github.com/NVIDIA/aicr/edit/main/docs/conformance/cncf/index.md'
          }
          return `https://github.com/NVIDIA/aicr/edit/main/site/${filePath}`
        },
        text: 'Edit this page on GitHub',
      },

      footer: {
        message: 'Released under the Apache 2.0 License.',
        copyright: 'Copyright © 2026 NVIDIA Corporation',
      },
    },
  })
)
