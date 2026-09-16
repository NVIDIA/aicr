// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Find YAML fences with the same Markdown parser used by the MDX docs gate,
// then parse the complete YAML stream inside every matching fence.

import { readFileSync } from 'node:fs'
import { createProcessor } from '@mdx-js/mdx'
import remarkFrontmatter from 'remark-frontmatter'
import remarkGfm from 'remark-gfm'
import { visit } from 'unist-util-visit'
import { Composer, LineCounter, Parser, visit as visitYaml } from 'yaml'
import remarkDocsRecovery from './remark-docs-recovery.mjs'

const files = process.argv.slice(2)
const maxDiagnosticsPerBlock = 3

if (files.length === 0) {
  console.error('usage: check-docs-yaml.mjs <file>...')
  process.exit(2)
}

// MDX exposes fences inside JSX components while keeping expressions and
// comments out of the code-block tree.
const mdxProcessor = createProcessor({
  format: 'mdx',
  remarkPlugins: [remarkFrontmatter, remarkGfm],
})

// If a file contains invalid MDX elsewhere, the full MDX parser cannot return
// an AST. Recovery keeps comments opaque and exposes fences in JSX wrappers.
const mdxRecoveryProcessor = createProcessor({
  format: 'md',
  remarkPlugins: [remarkDocsRecovery, remarkFrontmatter, remarkGfm],
})

let checkedBlocks = 0
let failedBlocks = 0
let failedFiles = 0

function oneLine(message) {
  return String(message).replace(/\s+/g, ' ').trim()
}

function diagnosticLocation(file, node, error, lineCounter) {
  const openingLine = node.position?.start.line || 1
  const openingColumn = node.position?.start.column || 1

  if (!Array.isArray(error.pos) || error.pos.length === 0) {
    return `${file}:${openingLine}:${openingColumn}`
  }

  const relative = lineCounter.linePos(error.pos[0])
  if (!relative) {
    return `${file}:${openingLine}:${openingColumn}`
  }

  // A code node starts on its opening fence, so YAML line 1 is the following
  // Markdown line. Include fence indentation in the reported source column.
  const line = openingLine + relative.line
  const column = openingColumn + relative.col - 1
  return `${file}:${line}:${column}`
}

function parseYamlStream(source, lineCounter) {
  const parser = new Parser(lineCounter.addNewLine)
  const composer = new Composer({ logLevel: 'error', prettyErrors: false, strict: true })
  const documents = []
  let pendingDirective

  // The high-level parser accepts dangling directives after a document. Track
  // directive tokens so every prelude must introduce a document, including at EOF.
  for (const token of parser.parse(source)) {
    if (token.type === 'directive') {
      pendingDirective ||= token
    } else if (token.type === 'document') {
      pendingDirective = undefined
    }
    documents.push(...composer.next(token))
  }
  documents.push(...composer.end())

  const errors = documents.flatMap(document => document.errors)
  errors.push(...composer.streamInfo().errors)
  if (pendingDirective) {
    errors.push({
      message: "directives must be followed by '---' to start a YAML document",
      pos: [pendingDirective.offset],
    })
  }
  return { documents, errors }
}

for (const file of files) {
  let source
  try {
    source = readFileSync(file, 'utf8')
  } catch (error) {
    console.log(`DOC-YAML: ${file}: cannot read: ${oneLine(error.message)}`)
    failedFiles++
    continue
  }

  let tree
  try {
    tree = mdxProcessor.parse(source)
  } catch (mdxError) {
    try {
      tree = mdxRecoveryProcessor.parse(source)
    } catch (recoveryError) {
      const mdxMessage = oneLine(mdxError.message)
      const recoveryMessage = oneLine(recoveryError.message)
      console.log(
        `DOC-YAML: ${file}: cannot discover MDX-nested code blocks after ` +
        `MDX parse error (${mdxMessage}): ${recoveryMessage}`
      )
      failedFiles++
      continue
    }
  }

  const codeNodes = []
  visit(tree, 'code', node => {
    codeNodes.push(node)
  })

  for (const node of codeNodes) {
    const language = (node.lang || '').toLowerCase()

    // Comma-suffixed labels have historically been used as ad-hoc opt-outs.
    // Fail them explicitly instead of silently treating them as another format.
    if (/^ya?ml,/.test(language)) {
      checkedBlocks++
      failedBlocks++
      const line = node.position?.start.line || 1
      const column = node.position?.start.column || 1
      console.log(`DOC-YAML: ${file}:${line}:${column}: unsupported YAML fence label '${node.lang}'; use yaml or yml`)
      continue
    }

    if (language !== 'yaml' && language !== 'yml') {
      continue
    }

    checkedBlocks++
    const lineCounter = new LineCounter()
    let stream

    try {
      stream = parseYamlStream(node.value, lineCounter)
    } catch (error) {
      failedBlocks++
      const line = node.position?.start.line || 1
      const column = node.position?.start.column || 1
      console.log(`DOC-YAML: ${file}:${line}:${column}: ${oneLine(error.message)}`)
      continue
    }

    const { documents, errors } = stream
    for (const document of documents) {
      if (document.errors.length > 0) {
        continue
      }
      // Visiting anchors before their children permits recursive aliases while
      // rejecting missing, forward, and cross-document references in one pass.
      const anchors = new Set()
      visitYaml(document, {
        Value(_key, node) {
          if (node.anchor) anchors.add(node.anchor)
        },
        Alias(_key, alias) {
          if (!anchors.has(alias.source)) {
            errors.push({
              message: `unresolved alias '${alias.source}'; its anchor must appear earlier in the same document`,
              pos: alias.range,
            })
          }
        },
      })
    }

    // yaml accepts a bare document after `...` as a new stream document. In a
    // documentation example this is almost always an ellipsis typo: the next
    // mapping silently becomes a separate object. Require an explicit `---`
    // when content intentionally resumes after an explicit document end.
    for (let index = 1; index < documents.length; index++) {
      const previous = documents[index - 1]
      const current = documents[index]
      if (previous.directives.docEnd && !current.directives.docStart) {
        errors.push({
          message: "content after '...' must start a new YAML document with '---'; use '# ...' for an elision",
          pos: [current.range?.[0] || 0],
        })
      }
    }

    if (errors.length === 0) {
      continue
    }

    failedBlocks++
    for (const error of errors.slice(0, maxDiagnosticsPerBlock)) {
      const where = diagnosticLocation(file, node, error, lineCounter)
      console.log(`DOC-YAML: ${where}: ${oneLine(error.message)}`)
    }

    const suppressedErrors = errors.length - maxDiagnosticsPerBlock
    if (suppressedErrors > 0) {
      const line = node.position?.start.line || 1
      const column = node.position?.start.column || 1
      console.log(
        `DOC-YAML: ${file}:${line}:${column}: ` +
        `${suppressedErrors} additional error(s) suppressed for this block`
      )
    }
  }
}

if (checkedBlocks === 0) {
  console.log('ERROR: no yaml/yml fenced code blocks were found.')
  console.log('The gate refuses to pass without checking at least one block.')
  process.exit(1)
}

if (failedBlocks > 0 || failedFiles > 0) {
  console.log('')
  console.log(`ERROR: ${failedBlocks} of ${checkedBlocks} YAML block(s) fail syntax parsing.`)
  if (failedFiles > 0) {
    console.log(`ERROR: ${failedFiles} Markdown file(s) could not be checked.`)
  }
  console.log('Use valid YAML in yaml/yml fences. Relabel templates or pseudocode as gotemplate or text.')
  process.exit(1)
}

console.log(`OK: ${checkedBlocks} YAML block(s) in ${files.length} Markdown file(s) parse successfully`)
