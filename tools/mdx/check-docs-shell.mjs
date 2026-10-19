// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Find shell fences with the same Markdown parser used by the MDX docs gate,
// then report every `<placeholder>` the shell would lex as a redirection.
//
// `kubectl delete namespace <namespace>` is not a command a reader can paste:
// bash reads `<namespace` as input redirection and `>` as output redirection.
// Rewriting it as a bare `${NAMESPACE}` is worse, because an unset variable
// expands to nothing and the command runs with an argument missing. The fix
// this gate asks for is an assignment line before first use plus a quoted
// expansion, or quoting the placeholder where it is only a label.

import { readFileSync } from 'node:fs'
import { createProcessor } from '@mdx-js/mdx'
import remarkFrontmatter from 'remark-frontmatter'
import remarkGfm from 'remark-gfm'
import { visit } from 'unist-util-visit'
import remarkDocsRecovery from './remark-docs-recovery.mjs'

// `console` is excluded: by convention it holds `$ ` prompts interleaved with
// output, which is illustrative rather than pasteable.
const shellLanguages = new Set(['bash', 'sh', 'shell', 'zsh'])

// A placeholder opens with `<` directly followed by a character that neither
// separates words nor forms a longer operator (`<<`, `<>`, `<(`, `<&`), and
// closes at the first `>` on the same line that ends a word: `<name>`,
// `<path|cm://...>`, `</path/to/file.yaml>`, `<your cluster name>`. A quote,
// escape, `;`, `&`, or comment ends the search first. Real redirections never
// close that way: `<file`, `< file`, `<&3`, `<(cmd)`, `<in >out` where the `>`
// starts a word, and `<in 2>err` where it follows a file descriptor.
const placeholderOpen = /^<[^\s<>(&]/
const placeholderStop = /[\n<'"`\\;&]/
const redirectionClose = /\s\d*>$/

// matchPlaceholder returns the placeholder token at the start of `text`, or
// null when the `<` there is a real redirection operator.
function matchPlaceholder(text) {
  if (!placeholderOpen.test(text)) {
    return null
  }
  for (let j = 1; j < text.length; j++) {
    const ch = text[j]
    if (ch === '>') {
      const token = text.slice(0, j + 1)
      return redirectionClose.test(token) ? null : token
    }
    if (placeholderStop.test(ch) || (ch === '#' && /\s/.test(text[j - 1]))) {
      return null
    }
  }
  return null
}

const files = process.argv.slice(2)

if (files.length === 0) {
  console.error('usage: check-docs-shell.mjs <file>...')
  process.exit(2)
}

const mdxProcessor = createProcessor({
  format: 'mdx',
  remarkPlugins: [remarkFrontmatter, remarkGfm],
})

const mdxRecoveryProcessor = createProcessor({
  format: 'md',
  remarkPlugins: [remarkDocsRecovery, remarkFrontmatter, remarkGfm],
})

function oneLine(message) {
  return String(message).replace(/\s+/g, ' ').trim()
}

// A word starts after whitespace, a control operator, or the start of input.
// Only there does `#` open a comment and `((` open an arithmetic command.
function atWordStart(source, index) {
  return index === 0 || /[\s;&|()]/.test(source[index - 1])
}

// skipQuoted returns the index just past the closing `quote`, honoring
// backslash escapes when `escapes` is set. An unterminated quote consumes the
// rest of the block, which matches how bash would keep reading.
function skipQuoted(source, start, quote, escapes) {
  let i = start
  while (i < source.length) {
    if (escapes && source[i] === '\\') {
      i += 2
      continue
    }
    if (source[i] === quote) {
      return i + 1
    }
    i++
  }
  return i
}

// skipBalanced returns the index just past the `close` that balances the
// `open` already consumed before `start`. Quotes inside are honored so a
// quoted `}` or `)` does not end the region early.
function skipBalanced(source, start, open, close) {
  let depth = 1
  let i = start
  while (i < source.length) {
    const ch = source[i]
    if (ch === '\\') {
      i += 2
      continue
    }
    if (ch === "'") {
      i = skipQuoted(source, i + 1, "'", false)
      continue
    }
    if (ch === '"') {
      i = skipQuoted(source, i + 1, '"', true)
      continue
    }
    if (ch === open) {
      depth++
    } else if (ch === close) {
      depth--
      if (depth === 0) {
        return i + 1
      }
    }
    i++
  }
  return i
}

// readHeredocDelimiter parses the word after `<<` or `<<-` and returns the
// delimiter text with quoting removed, whether any part of it was quoted, and
// the index just past the word.
function readHeredocDelimiter(source, start) {
  let i = start
  while (source[i] === ' ' || source[i] === '\t') {
    i++
  }
  let word = ''
  let quoted = false
  while (i < source.length && !/[\s;&|()<>]/.test(source[i])) {
    const ch = source[i]
    if (ch === "'" || ch === '"') {
      const end = skipQuoted(source, i + 1, ch, ch === '"')
      word += source.slice(i + 1, end - 1)
      quoted = true
      i = end
      continue
    }
    if (ch === '\\') {
      word += source[i + 1] || ''
      quoted = true
      i += 2
      continue
    }
    word += ch
    i++
  }
  return { word, quoted, end: i }
}

// scanHeredocBodies consumes the bodies of every heredoc opened on the line
// that just ended at `start` and returns the index past the last delimiter.
// A quoted delimiter makes the body literal data. An unquoted one makes bash
// expand it like a double-quoted string, so command substitution inside it
// is lexed and its placeholders are appended to `found`.
function scanHeredocBodies(source, start, pending, found) {
  let i = start
  for (const { word, quoted, stripTabs } of pending) {
    const bodyStart = i
    let bodyEnd = source.length
    while (i < source.length) {
      let end = source.indexOf('\n', i)
      if (end === -1) {
        end = source.length
      }
      let line = source.slice(i, end)
      if (stripTabs) {
        line = line.replace(/^\t+/, '')
      }
      const lineStart = i
      i = end + 1
      if (line === word) {
        bodyEnd = lineStart
        break
      }
    }
    if (!quoted) {
      for (const hit of findPlaceholders(source.slice(bodyStart, bodyEnd), 'heredoc')) {
        found.push({ index: bodyStart + hit.index, token: hit.token })
      }
    }
  }
  return i
}

// findPlaceholders lexes a shell block and returns the offset and text of
// every unquoted `<placeholder>`. It models the parts of the shell grammar that
// decide whether a `<` reaches the lexer as an operator: quoting, escapes,
// comments, heredoc bodies, parameter expansion, and command and arithmetic
// substitution, including command substitution inside double quotes and
// unquoted heredoc bodies. `context` is 'heredoc' when `source` is such a body.
function findPlaceholders(source, context = 'shell') {
  const found = []
  const pendingHeredocs = []
  // Each frame is the context a closing token returns to: 'cmd' for shell
  // syntax (top level, `$(...)`, backticks) and 'dq' for double-quote rules,
  // which also govern an unquoted heredoc body, where `"` closes nothing.
  const stack = context === 'heredoc'
    ? [{ kind: 'dq', close: null }]
    : [{ kind: 'cmd', close: null }]
  let i = 0

  while (i < source.length) {
    const frame = stack[stack.length - 1]
    const ch = source[i]
    const next = source[i + 1]

    if (ch === '\\') {
      i += 2
      continue
    }

    if (frame.kind === 'dq') {
      if (ch === frame.close) {
        stack.pop()
        i++
      } else if (ch === '$' && next === '(' && source[i + 2] === '(') {
        i = skipBalanced(source, i + 2, '(', ')')
      } else if (ch === '$' && next === '(') {
        stack.push({ kind: 'cmd', close: ')' })
        i += 2
      } else if (ch === '$' && next === '{') {
        i = skipBalanced(source, i + 2, '{', '}')
      } else if (ch === '`') {
        stack.push({ kind: 'cmd', close: '`' })
        i++
      } else {
        i++
      }
      continue
    }

    if (ch === '\n') {
      i++
      if (pendingHeredocs.length > 0) {
        i = scanHeredocBodies(source, i, pendingHeredocs, found)
        pendingHeredocs.length = 0
      }
      continue
    }

    if (frame.close !== null && ch === frame.close) {
      stack.pop()
      i++
      continue
    }

    if (ch === "'") {
      i = skipQuoted(source, i + 1, "'", false)
    } else if (ch === '$' && next === "'") {
      i = skipQuoted(source, i + 2, "'", true)
    } else if (ch === '"') {
      stack.push({ kind: 'dq', close: '"' })
      i++
    } else if (ch === '#' && atWordStart(source, i)) {
      const end = source.indexOf('\n', i)
      i = end === -1 ? source.length : end
    } else if (ch === '$' && next === '(' && source[i + 2] === '(') {
      i = skipBalanced(source, i + 2, '(', ')')
    } else if (ch === '(' && next === '(' && atWordStart(source, i)) {
      i = skipBalanced(source, i + 1, '(', ')')
    } else if (ch === '$' && next === '(') {
      stack.push({ kind: 'cmd', close: ')' })
      i += 2
    } else if (ch === '$' && next === '{') {
      i = skipBalanced(source, i + 2, '{', '}')
    } else if (ch === '`') {
      stack.push({ kind: 'cmd', close: '`' })
      i++
    } else if (ch === '(' || (ch === '<' && next === '(') || (ch === '>' && next === '(')) {
      // Subshells and process substitutions nest shell syntax under `)`.
      const width = ch === '(' ? 1 : 2
      stack.push({ kind: 'cmd', close: ')' })
      i += width
    } else if (ch === '<' && next === '<' && source[i + 2] === '<') {
      i += 3
    } else if (ch === '<' && next === '<') {
      const stripTabs = source[i + 2] === '-'
      const { word, quoted, end } = readHeredocDelimiter(source, i + (stripTabs ? 3 : 2))
      if (word !== '') {
        pendingHeredocs.push({ word, quoted, stripTabs })
      }
      i = end
    } else if (ch === '<') {
      const token = matchPlaceholder(source.slice(i))
      if (token !== null) {
        found.push({ index: i, token })
        i += token.length
      } else {
        i++
      }
    } else {
      i++
    }
  }

  return found
}

let checkedBlocks = 0
let violations = 0
let failedFiles = 0

for (const file of files) {
  let source
  try {
    source = readFileSync(file, 'utf8')
  } catch (error) {
    console.log(`DOC-SHELL: ${file}: cannot read: ${oneLine(error.message)}`)
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
        `DOC-SHELL: ${file}: cannot discover MDX-nested code blocks after ` +
        `MDX parse error (${mdxMessage}): ${recoveryMessage}`
      )
      failedFiles++
      continue
    }
  }

  visit(tree, 'code', node => {
    if (!shellLanguages.has((node.lang || '').toLowerCase())) {
      return
    }
    checkedBlocks++

    const lines = node.value.split('\n')
    const lineStarts = []
    let offset = 0
    for (const line of lines) {
      lineStarts.push(offset)
      offset += line.length + 1
    }

    // A code node starts on its opening fence, so block line 0 is the
    // following Markdown line. The column adds back the fence indentation the
    // parser stripped from the value.
    const openingLine = node.position?.start.line || 1
    const indent = (node.position?.start.column || 1) - 1
    for (const { index, token } of findPlaceholders(node.value)) {
      let row = lineStarts.length - 1
      while (lineStarts[row] > index) {
        row--
      }
      const column = indent + index - lineStarts[row] + 1
      console.log(`DOC-SHELL: ${file}:${openingLine + row + 1}:${column}: unquoted placeholder ${token} is shell redirection: ${lines[row].trim()}`)
      violations++
    }
  })
}

if (checkedBlocks === 0) {
  console.log('ERROR: no bash/sh/shell/zsh fenced code blocks were found.')
  console.log('The gate refuses to pass without checking at least one block.')
  process.exit(1)
}

if (violations > 0 || failedFiles > 0) {
  console.log('')
  if (violations > 0) {
    console.log(`ERROR: ${violations} unquoted <placeholder>(s) in shell examples; bash reads them as redirection.`)
    console.log('Assign a variable before first use and quote the expansion:')
    console.log('  NAMESPACE="gpu-operator"  # replace with your namespace')
    console.log('  kubectl get pods -n "${NAMESPACE}"')
    console.log('Never emit a bare ${VAR} without its assignment: an unset variable expands to nothing and the command still runs.')
    console.log('Relabel output and prompt-style transcripts as text or console.')
  }
  if (failedFiles > 0) {
    console.log(`ERROR: ${failedFiles} Markdown file(s) could not be checked.`)
  }
  process.exit(1)
}

console.log(`OK: ${checkedBlocks} shell block(s) in ${files.length} Markdown file(s) contain no unquoted placeholders`)
