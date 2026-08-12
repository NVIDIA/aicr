// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Parser-level MDX validation for docs/ — the authoritative gate.
//
// Fern renders our markdown through MDX, so a construct that is valid
// CommonMark can still abort `fern generate --docs` at publish time. This
// script runs the SAME parser Fern does (@mdx-js/mdx, locked in
// ./package-lock.json) over every published doc and reports the first parse
// error per file with file:line:column and the parser's own message.
//
// It lives beside package.json so Node's ESM resolver finds ./node_modules by
// walking up from this file. Do not invoke directly — `tools/check-docs-mdx-parse`
// installs the locked tree first and passes the files to check as argv.
//
// Relationship to tools/check-docs-mdx: that script is a fast, dependency-free
// bash approximation kept for the `make lint` fast path. THIS script is the
// ground truth. Where they disagree, this one is right — and the bash rules
// are deliberately kept to a strict subset so they never reject content MDX
// accepts.

import { readFileSync } from 'node:fs'
import { compile } from '@mdx-js/mdx'
import remarkFrontmatter from 'remark-frontmatter'
import remarkGfm from 'remark-gfm'

const files = process.argv.slice(2)

if (files.length === 0) {
  console.error('usage: check-docs-mdx-parse.mjs <file>...')
  process.exit(2)
}

// format: 'mdx' is what makes this faithful. MDX's 'md' format disables JSX
// and expression parsing entirely, which would silently accept every hazard
// this gate exists to catch. Fern parses .md as MDX, so we must too.
//
// The plugin set must mirror what Fern renders with, or the gate drifts from
// the publish step in one direction or the other:
//
//   remark-gfm         — tables, autolink literals, strikethrough. GFM changes
//                        tokenization, so a '<' inside a table cell or next to
//                        an autolink can parse differently with and without it.
//   remark-frontmatter — Fern strips YAML frontmatter before MDX sees it.
//                        Without this plugin the delimiters parse as a thematic
//                        break and the frontmatter body as prose, so a title
//                        like `gate <= 2,000` was rejected here while Fern
//                        published it happily. That is the false-positive
//                        direction this gate must never take.
const options = {
  format: 'mdx',
  remarkPlugins: [remarkFrontmatter, remarkGfm],
}

let failed = 0

for (const file of files) {
  let source
  try {
    source = readFileSync(file, 'utf8')
  } catch (err) {
    console.log(`MDX-PARSE: ${file}: cannot read: ${err.message}`)
    failed++
    continue
  }

  try {
    await compile(source, options)
  } catch (err) {
    // VFileMessage carries line/column and a `reason` free of the file prefix;
    // fall back to message for anything that is not a parse diagnostic.
    const where = err.line ? `${file}:${err.line}:${err.column}` : file
    console.log(`MDX-PARSE: ${where}: ${err.reason || err.message}`)
    failed++
  }
}

if (failed > 0) {
  console.log('')
  console.log(`ERROR: ${failed} of ${files.length} doc file(s) fail MDX parsing.`)
  console.log('')
  console.log('These WILL break `fern generate --docs` at publish time.')
  console.log('Common fixes:')
  console.log('  gate <= 2,000        → gate `<= 2,000`   (wrap in a code span)')
  console.log('  <30 s                → `<30 s`           or &lt;30 s')
  console.log('  a stray </div> or <> → remove it, or wrap in a code span')
  console.log('  <br>                 → <br />            (self-close void elements)')
  console.log('  {template}           → \\{template\\}')
  process.exit(1)
}

console.log(`OK: ${files.length} doc file(s) parse as MDX`)
