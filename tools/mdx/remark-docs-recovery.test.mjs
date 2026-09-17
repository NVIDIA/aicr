// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict'
import test from 'node:test'
import { createProcessor } from '@mdx-js/mdx'
import remarkFrontmatter from 'remark-frontmatter'
import remarkGfm from 'remark-gfm'
import { visit } from 'unist-util-visit'
import remarkDocsRecovery from './remark-docs-recovery.mjs'

test('recovery preserves the MDX parser’s visible YAML fences and source lines', () => {
  const mdx = createProcessor({ format: 'mdx', remarkPlugins: [remarkFrontmatter, remarkGfm] })
  const recovery = createProcessor({
    format: 'md',
    remarkPlugins: [remarkDocsRecovery, remarkFrontmatter, remarkGfm],
  })
  const visible = '```yaml\nvisible: true\n```\n'
  const broken = '```yaml\nbroken: [\n```\n'
  const examples = {
    blockComment: '{/*\n' + broken + '*/}\n\n' + visible,
    unclosedFenceInComment: '{/*\n```yaml\nbroken: [\n*/}\n\n' + visible,
    multipleComments: '{ /* } { */ /* more */ }\n\n' + visible,
    lineComment: '{ // comment\n/*\n' + broken + '*/ }\n\n' + visible,
    escapedMdx: '\\{/*\n\n' + broken + '\n*/}\n\n' + visible,
    escapedHtml: '\\<!--\n\n' + broken + '\n-->\n\n' + visible,
    inlineCode: 'Literal `{/*` and `<!--`.\n\n' + visible,
    literalFence: '````text\n{/*\n' + broken + '*/}\n````\n\n' + visible,
    unicodeSpace: '{\u00a0/*\n' + broken + '*/\u00a0}\n\n' + visible,
    jsx: '<Tabs>\n' + visible + '</Tabs>\n',
    expressionWithContent: '{/* explanation */ 1}\n\n' + visible,
    noBlankAfterComment: '{/* comment */}\n' + visible,
    sameLineComment: '{/* comment */} and more text.\n\n' + visible,
    starRuns: '{/***** stars ***/}\n\n' + visible,
    adjacent: '{/* one */}{/* two */}\n\n' + visible,
    adjacentJsx: '<Tabs>{/*\n' + broken + '*/}</Tabs>\n\n' + visible,
  }
  const wrappers = {
    plain: source => source,
    quoted: source => source.split('\n').map(line => '> ' + line).join('\n'),
    listed: source => '- ' + source.split('\n').join('\n  '),
    jsx: source => '<Tabs>\n\n' + source + '\n</Tabs>\n',
    frontmatter: source => '---\ntitle: example\n---\n\n' + source,
  }

  for (const [name, example] of Object.entries(examples)) {
    for (const [container, wrap] of Object.entries(wrappers)) {
      for (const newline of ['\n', '\r\n']) {
        const source = wrap(example).replaceAll('\n', newline)
        assert.deepEqual(
          yamlBlocks(recovery.parse(source)),
          yamlBlocks(mdx.parse(source)),
          `${name} inside ${container}, newline ${JSON.stringify(newline)}`,
        )
      }
    }
  }
})

test('unterminated comments cannot consume examples outside their container', () => {
  const recovery = createProcessor({ format: 'md', remarkPlugins: [remarkDocsRecovery] })
  const containers = [['> ', '> '], ['- ', '  ']]
  const comments = [['{/*', '*/}'], ['<!--', '-->']]
  for (const [first, next] of containers) {
    for (const [open, close] of comments) {
      const source = first + open + '\n' + next + 'hidden\n\n' +
        '```yaml\nbroken: [\n```\n\n' + close + '\n'
      assert.deepEqual(yamlBlocks(recovery.parse(source)), [{ line: 4, value: 'broken: [' }])
    }
  }
})

function yamlBlocks(tree) {
  const blocks = []
  visit(tree, 'code', node => {
    if (node.lang === 'yaml' || node.lang === 'yml') {
      blocks.push({ line: node.position.start.line, value: node.value })
    }
  })
  return blocks
}
