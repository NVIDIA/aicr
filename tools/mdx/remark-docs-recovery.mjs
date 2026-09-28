// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { htmlFlow, htmlText } from 'micromark-core-commonmark'
import { markdownLineEnding, markdownLineEndingOrSpace, markdownSpace } from 'micromark-util-character'
import { codes } from 'micromark-util-symbol'

// Keep comments opaque while allowing fences inside JSX-like wrappers in
// otherwise plain Markdown. Micromark handles escapes and literal code before
// invoking these constructs; no source masking or repeated parsing is needed.
export default function remarkDocsRecovery() {
  const data = this.data()
  const extensions = data.micromarkExtensions || (data.micromarkExtensions = [])
  extensions.push({
    disable: { null: ['htmlFlow', 'htmlText'] },
    flow: {
      [codes.lessThan]: recoveryFlow,
      [codes.leftCurlyBrace]: recoveryFlow,
    },
    text: {
      [codes.lessThan]: htmlComment(htmlText),
      [codes.leftCurlyBrace]: mdxComment(false),
    },
  })
}

// Consume adjacent tags and comments together, including <Tabs>{/* ... */}.
// Standalone tags end at the newline rather than opening raw HTML blocks.
const recoveryFlow = { concrete: true, tokenize: tokenizeRecoveryFlow }
const flowMarkup = [htmlComment(htmlFlow), { ...htmlText, name: 'docsHtmlTag' }]
const flowMdxComment = mdxComment(true)

function tokenizeRecoveryFlow(effects, ok, nok) {
  return start

  function start(code) {
    if (code === codes.lessThan) return effects.attempt(flowMarkup, after, nok)(code)
    if (code === codes.leftCurlyBrace) return effects.attempt(flowMdxComment, after, nok)(code)
    return nok(code)
  }

  function after(code) {
    if (code === codes.eof || markdownLineEnding(code)) return ok(code)
    if (markdownSpace(code)) {
      effects.enter('whitespace')
      effects.consume(code)
      effects.exit('whitespace')
      return after
    }
    return start(code)
  }
}

function htmlComment(construct) {
  return {
    ...construct,
    name: `${construct.name}Comment`,
    tokenize(effects, ok, nok) {
      const start = construct.tokenize.call(this, effects, ok, nok)
      return effects.check({ tokenize: htmlCommentStart }, start, nok)
    },
  }
}

function htmlCommentStart(effects, ok, nok) {
  const marker = '<!--'
  let index = 0
  return match

  function match(code) {
    if (code !== marker.charCodeAt(index)) return nok(code)
    effects.consume(code)
    index++
    return index === marker.length ? ok : match
  }
}

// Only whitespace and JS comments may occur between the braces. Any expression
// with other content falls back to Markdown, so recovery cannot hide examples
// by treating arbitrary braces as an invisible MDX expression.
function mdxComment(flow) {
  return { concrete: flow, tokenize }

  function tokenize(effects, ok, nok) {
    const context = this
    let hasComment = false
    let continuation
    return start

    function start(code) {
      effects.enter('docsMdxComment')
      effects.consume(code)
      return betweenComments
    }

    function betweenComments(code) {
      if (markdownLineEndingOrSpace(code) || (code > 0 && /\s/u.test(String.fromCodePoint(code)))) {
        return consume(code, betweenComments)
      }
      if (code === codes.slash) {
        effects.consume(code)
        return commentStart
      }
      if (code === codes.rightCurlyBrace && hasComment) {
        effects.consume(code)
        effects.exit('docsMdxComment')
        return ok
      }
      return nok(code)
    }

    function commentStart(code) {
      if (code === codes.asterisk || code === codes.slash) {
        effects.consume(code)
        return code === codes.asterisk ? blockComment : lineComment
      }
      return nok(code)
    }

    function blockComment(code) {
      if (code === codes.eof) return nok(code)
      return consume(code, code === codes.asterisk ? blockCommentEnd : blockComment)
    }

    function blockCommentEnd(code) {
      if (code === codes.slash) {
        effects.consume(code)
        hasComment = true
        return betweenComments
      }
      return blockComment(code)
    }

    function lineComment(code) {
      if (code === codes.eof) return nok(code)
      if (markdownLineEnding(code)) {
        hasComment = true
        return betweenComments(code)
      }
      effects.consume(code)
      return lineComment
    }

    function consume(code, next) {
      if (markdownLineEnding(code)) {
        effects.enter('lineEnding')
        effects.consume(code)
        effects.exit('lineEnding')
        continuation = next
        return afterLine
      } else {
        effects.consume(code)
        return next
      }
    }

    function afterLine(code) {
      // A flow comment cannot continue beyond its blockquote or list prefix.
      if (flow && context.parser.lazy[context.now().line]) return nok(code)
      return continuation(code)
    }
  }
}
