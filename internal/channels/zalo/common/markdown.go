// Package common holds shared building blocks used by both Zalo channel
// flavors (zalo_bot and zalo_oa). Anything that is *not* genuinely shared
// (HTTP API clients, send pipelines, auth) stays in the per-channel package.
package common

import (
	"regexp"
	"strings"
)

// StripMarkdown removes markdown formatting artifacts from text, producing
// clean plain text suitable for Zalo which does not support any markup.
func StripMarkdown(text string) string {
	if text == "" {
		return text
	}

	text = reFencedCode.ReplaceAllString(text, "$1")
	text = reInlineCode.ReplaceAllString(text, "$1")
	text = reImage.ReplaceAllString(text, "")
	text = reLink.ReplaceAllString(text, "$1 ($2)")
	text = reBoldItalicStar.ReplaceAllString(text, "$1")
	text = reBoldItalicUnder.ReplaceAllString(text, "$1")
	text = reBoldStar.ReplaceAllString(text, "$1")
	text = reBoldUnder.ReplaceAllString(text, "$1")
	text = reStrikethrough.ReplaceAllString(text, "$1")
	text = reHeader.ReplaceAllString(text, "$1")
	text = reHorizontalRule.ReplaceAllString(text, "")
	text = reBlockquote.ReplaceAllString(text, "$1")
	text = reBullet.ReplaceAllString(text, "${1}• ")
	text = reExcessiveNewlines.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}

var (
	reFencedCode      = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\\n?(.*?)```")
	reInlineCode      = regexp.MustCompile("`([^`]+)`")
	reImage           = regexp.MustCompile(`!\[[^\]]*\]\([^)]+\)`)
	reLink            = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBoldItalicStar  = regexp.MustCompile(`\*{3}(.+?)\*{3}`)
	reBoldItalicUnder = regexp.MustCompile(`_{3}(.+?)_{3}`)
	reBoldStar        = regexp.MustCompile(`\*{2}(.+?)\*{2}`)
	reBoldUnder       = regexp.MustCompile(`_{2}(.+?)_{2}`)
	reStrikethrough   = regexp.MustCompile(`~~(.+?)~~`)
	reHeader          = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reHorizontalRule  = regexp.MustCompile(`(?m)^[\s]*[-*_]{3,}[\s]*$`)
	reBlockquote      = regexp.MustCompile(`(?m)^>\s?(.*)$`)
	reBullet          = regexp.MustCompile(`(?m)^(\s*)[-*+]\s+`)

	reExcessiveNewlines = regexp.MustCompile(`\n{3,}`)
)
