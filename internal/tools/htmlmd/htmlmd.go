// Package htmlmd converts an HTML page to Markdown for the web_fetch tool.
//
// It is a port of github.com/JohannesKaufmann/html-to-markdown v1.6.0 —
// NewConverter("", true, nil).ConvertString, the CommonMark rule set with the
// default options (atx headings, "-" bullets, fenced code with ```, _ and **,
// inlined links, basic escaping, no domain) — re-expressed over
// golang.org/x/net/html nodes instead of goquery, so neither goquery nor
// cascadia is needed. Everything else the library offered (plugins, Keep/
// Remove, referenced links, the other option values, URL fetching) is dropped.
// The output is byte-identical to the library's on the golden corpus in
// testdata/ (generated with the library before it was removed), quirks
// included: this is the text a model has been reading, and changing it is a
// separate decision.
//
// Copyright (c) 2018 Johannes Kaufmann; see LICENSE. Modified: ported from
// goquery selections to *html.Node, trimmed to the defaults fleet uses.
// SPDX-License-Identifier: MIT
package htmlmd

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// The library's default options, as constants.
const (
	bulletListMarker = "-"
	horizontalRule   = "* * *"
	fence            = "```"
	emDelimiter      = "_"
	strongDelimiter  = "**"
)

// attrListPrefix is the attribute the library stamped on every <li> before
// converting, holding its list marker. It is kept as an attribute (not a side
// table) because the indentation rule reads it from whatever element is a
// list's first child, which a page can set itself.
const attrListPrefix = "data-converter-list-prefix"

var (
	multipleNewLinesRegex       = regexp.MustCompile(`[\n]{2,}`)
	multipleSpacesR             = regexp.MustCompile(`  +`)
	leadingNewlinesR            = regexp.MustCompile(`^\n+`)
	trailingNewlinesR           = regexp.MustCompile(`\n+$`)
	tabR                        = regexp.MustCompile(`\t+`)
	multipleNewLinesInLinkRegex = regexp.MustCompile(`(\n\\){1,}`)
	lineStartR                  = regexp.MustCompile(`(?m)^`)
)

// Convert parses htmlContent and returns it as Markdown.
func Convert(htmlContent string) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", err
	}
	// The library's before-hook: stamp each <li> with its marker, computed on
	// the unconverted tree, in document order.
	forEachDescendant(doc, func(n *html.Node) {
		if isElement(n, "li") {
			setAttr(n, attrListPrefix, listPrefix(n))
		}
	})

	markdown := convertChildren(doc)

	// The library's after-hook.
	markdown = strings.TrimSpace(markdown)
	markdown = multipleNewLinesRegex.ReplaceAllString(markdown, "\n\n")
	return trimTrailingSpaces(markdown), nil
}

// convertChildren is the library's selecToMD: every child node (text and
// comments included) through its rule, concatenated.
func convertChildren(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if md, ok := convertNode(c); ok {
			b.WriteString(md)
		}
	}
	return b.String()
}

// convertNode applies the rule for n's node name. ok is false when the rule
// skips n (writes nothing).
func convertNode(n *html.Node) (string, bool) {
	name := nodeName(n)
	switch name {
	case "script", "style", "textarea": // removed outright
		return "", false
	case "code", "kbd", "samp", "tt":
		return codeRule(n), true
	case "pre":
		return preRule(n), true
	}
	content := convertChildren(n)
	switch name {
	case "ul", "ol":
		return listRule(content, n), true
	case "li":
		return listItemRule(content, n)
	case "#text":
		return textRule(n), true
	case "p", "div":
		return paragraphRule(content, n), true
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return headingRule(content, n)
	case "strong", "b":
		return delimitedRule(content, n, strongDelimiter, "strong", "b"), true
	case "i", "em":
		return delimitedRule(content, n, emDelimiter, "i", "em"), true
	case "img":
		return imageRule(n), true
	case "a":
		return linkRule(content, n)
	case "hr":
		if hasAncestor(n, "h1", "h2", "h3", "h4", "h5", "h6") {
			return "", true
		}
		return "\n\n" + horizontalRule + "\n\n", true
	case "br":
		return "\n\n", true
	case "blockquote":
		return blockquoteRule(content)
	case "noscript":
		return "", false
	}
	return content, true
}

func listRule(content string, n *html.Node) string {
	parent := parentElement(n)
	// A nested list: the ul/ol is the last element inside a list item.
	if (isElement(parent, "li") || isElement(parent, "ul") || isElement(parent, "ol")) && lastElementChild(parent) == n {
		// Put every list item on its own line.
		lastContentTextNode := strings.TrimRight(parent.FirstChild.Data, " \t")
		if !strings.HasSuffix(lastContentTextNode, "\n") {
			content = "\n" + content
		}
		// Remove empty lines between lists.
		if strings.HasSuffix(strings.TrimRight(content, " \t"), "\n") {
			content = strings.TrimRightFunc(content, unicode.IsSpace)
		}
		return content
	}
	return "\n\n" + content + "\n\n"
}

func listItemRule(content string, n *html.Node) (string, bool) {
	if strings.TrimSpace(content) == "" {
		return "", false
	}
	content = leadingNewlinesR.ReplaceAllString(content, "")
	content = trailingNewlinesR.ReplaceAllString(content, "\n")
	content = strings.TrimLeft(content, " ")

	prefix, _ := getAttr(n, attrListPrefix)
	// prefixCount is the space reserved for the siblings' markers, not
	// necessarily len(prefix).
	prefixCount, previousPrefixCounts := countListParents(n)
	if prefix == "" {
		prefix = strings.Repeat(" ", prefixCount)
	}
	prefix = strings.Repeat(" ", previousPrefixCounts) + prefix

	content = indentMultiLineListItem(content, prefixCount+previousPrefixCounts)
	return prefix + content + "\n", true
}

func textRule(n *html.Node) string {
	text := n.Data
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = tabR.ReplaceAllString(text, " ")
	// One space for many: indented text must not become a code block.
	text = multipleSpacesR.ReplaceAllString(text, " ")
	text = escapeMarkdown(text)

	// Inside a list, before a nested list: trim spaces (not newlines) so the
	// indentation survives.
	parent := parentElement(n)
	next := nextElementSibling(n)
	if indexWithText(n) == 0 &&
		(isElement(parent, "li") || isElement(parent, "ol") || isElement(parent, "ul")) &&
		(isElement(next, "ul") || isElement(next, "ol")) {
		text = strings.Trim(text, ` `)
	}
	return text
}

func paragraphRule(content string, n *html.Node) string {
	parent := nodeName(parentElement(n))
	if isInlineElement(parent) || parent == "li" {
		return "\n" + content + "\n"
	}
	return "\n\n" + trimLeadingSpaces(content) + "\n\n"
}

func headingRule(content string, n *html.Node) (string, bool) {
	if strings.TrimSpace(content) == "" {
		return "", false
	}
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.ReplaceAll(content, "\r", " ")
	content = strings.ReplaceAll(content, `#`, `\#`)
	content = strings.TrimSpace(content)

	if hasAncestor(n, "a") {
		return addSpaceIfNecessary(n, strongDelimiter+content+strongDelimiter), true
	}
	level, err := strconv.Atoi(n.Data[1:])
	if err != nil {
		return "", false
	}
	return "\n\n" + strings.Repeat("#", level) + " " + content + "\n\n", true
}

// delimitedRule is the strong/b and i/em rule: one pair of delimiters per
// line, the inner tag of a same-kind nesting left bare.
func delimitedRule(content string, n *html.Node, delimiter string, same ...string) string {
	if isElement(parentElement(n), same...) {
		return content
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return trimmed
	}
	// A newline between the delimiters stops them being recognized, so every
	// line with content gets its own pair.
	trimmed = delimiterForEveryLine(trimmed, delimiter)
	// Always have a space beside the delimiter, so it is recognized.
	return addSpaceIfNecessary(n, trimmed)
}

func imageRule(n *html.Node) string {
	src, _ := getAttr(n, "src")
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	alt, _ := getAttr(n, "alt")
	alt = strings.ReplaceAll(alt, "\n", " ")
	return fmt.Sprintf("![%s](%s)", alt, src)
}

func linkRule(content string, n *html.Node) (string, bool) {
	href, ok := getAttr(n, "href")
	if !ok || strings.TrimSpace(href) == "" || strings.TrimSpace(href) == "#" {
		return content, true
	}
	// Multi-line content inside a link.
	content = escapeMultiLine(content)

	var title string
	if t, ok := getAttr(n, "title"); ok {
		t = strings.ReplaceAll(t, "\n", " ")
		t = strings.ReplaceAll(t, `"`, `\"`)
		title = fmt.Sprintf(` "%s"`, t)
	}
	// No text (an icon, say): fall back to title, then aria-label.
	if strings.TrimSpace(content) == "" {
		if t, ok := getAttr(n, "title"); ok {
			content = t
		} else {
			content, _ = getAttr(n, "aria-label")
		}
	}
	if content == "" {
		return "", false
	}
	return addSpaceIfNecessary(n, fmt.Sprintf("[%s](%s%s)", content, href, title)), true
}

func codeRule(n *html.Node) string {
	code := codeContent(n)
	// Inline code: more than one newline in a row would end it.
	code = multipleNewLinesRegex.ReplaceAllString(code, "\n")

	fenceChar := '`'
	f := strings.Repeat(string(fenceChar), codeFenceOccurrences(fenceChar, code)+1)
	if strings.HasPrefix(code, "`") {
		code = " " + code
	}
	if strings.HasSuffix(code, "`") {
		code += " "
	}
	return addSpaceIfNecessary(n, f+code+f)
}

func preRule(n *html.Node) string {
	language := ""
	if codeElement := firstDescendant(n, "code"); codeElement != nil {
		language, _ = getAttr(codeElement, "class")
	}
	language = strings.Replace(language, "language-", "", 1)

	code := codeContent(n)
	fenceChar, _ := utf8.DecodeRuneInString(fence)
	f := calculateCodeFence(fenceChar, code)
	return "\n\n" + f + language + "\n" + code + "\n" + f + "\n\n"
}

func blockquoteRule(content string) (string, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", false
	}
	content = multipleNewLinesRegex.ReplaceAllString(content, "\n\n")
	content = lineStartR.ReplaceAllString(content, "> ")
	return "\n\n" + content + "\n\n", true
}

// --- node helpers: the goquery operations the rules used ---

// nodeName is goquery.NodeName for one node.
func nodeName(n *html.Node) string {
	if n == nil {
		return ""
	}
	switch n.Type {
	case html.ElementNode, html.DoctypeNode:
		return n.Data
	case html.ErrorNode:
		return "#error"
	case html.TextNode:
		return "#text"
	case html.DocumentNode:
		return "#document"
	case html.CommentNode:
		return "#comment"
	default: // html.RawNode has no name in goquery either
		return ""
	}
}

// isElement reports whether n is an element named one of names (a goquery
// tag selector: element nodes only, nil never matches).
func isElement(n *html.Node, names ...string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	for _, name := range names {
		if n.Data == name {
			return true
		}
	}
	return false
}

// parentElement is goquery's Parent: the parent if it is an element.
func parentElement(n *html.Node) *html.Node {
	if n.Parent != nil && n.Parent.Type == html.ElementNode {
		return n.Parent
	}
	return nil
}

func nextElementSibling(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.ElementNode {
			return s
		}
	}
	return nil
}

func firstElementChild(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

func lastElementChild(n *html.Node) *html.Node {
	for c := n.LastChild; c != nil; c = c.PrevSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

// elementIndex is goquery's Index: how many element siblings precede n.
func elementIndex(n *html.Node) int {
	i := 0
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type == html.ElementNode {
			i++
		}
	}
	return i
}

// hasAncestor is goquery's ParentsFiltered(names).Length() > 0.
func hasAncestor(n *html.Node, names ...string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if isElement(p, names...) {
			return true
		}
	}
	return false
}

func forEachDescendant(n *html.Node, fn func(*html.Node)) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		fn(c)
		forEachDescendant(c, fn)
	}
}

// firstDescendant is goquery's Find(name) narrowed to its first match.
func firstDescendant(n *html.Node, name string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if isElement(c, name) {
			return c
		}
		if d := firstDescendant(c, name); d != nil {
			return d
		}
	}
	return nil
}

// getAttr is goquery's Attr: the first attribute with that key.
func getAttr(n *html.Node, key string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// setAttr is goquery's SetAttr: replace the first attribute with that key, or
// append one.
func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

// collectText is goquery's Text: every descendant text node, concatenated.
func collectText(n *html.Node) string {
	var buf bytes.Buffer
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			buf.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return buf.String()
}

// --- the library's utils.go, on nodes ---

// inlineElements are the elements a <p>/<div> inside does not break out of.
var inlineElements = map[string]bool{
	"b": true, "big": true, "i": true, "small": true, "tt": true,
	"abbr": true, "acronym": true, "cite": true, "code": true, "dfn": true, "em": true, "kbd": true, "strong": true, "samp": true, "var": true,
	"a": true, "bdo": true, "br": true, "img": true, "map": true, "object": true, "q": true, "script": true, "span": true, "sub": true, "sup": true,
	"button": true, "input": true, "label": true, "select": true, "textarea": true,
}

func isInlineElement(name string) bool { return inlineElements[name] }

// isTrimmedElement: elements whose rule trims its own content, so a
// neighbour need not add a second space.
func isTrimmedElement(name string) bool {
	switch name {
	case "a", "strong", "b", "i", "em", "del", "s", "strike", "code":
		return true
	}
	return false
}

func prevNodeText(node *html.Node) (string, bool) {
	for ; node != nil; node = node.PrevSibling {
		text := collectText(node)
		name := nodeName(node)
		if name == "br" {
			return "\n", true
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if isTrimmedElement(name) {
			text = strings.TrimSpace(text)
		}
		return text, true
	}
	return "", false
}

func nextNodeText(node *html.Node) (string, bool) {
	for ; node != nil; node = node.NextSibling {
		text := collectText(node)
		name := nodeName(node)
		if name == "br" {
			return "\n", true
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		// "a a a": only one side adds the space.
		if isTrimmedElement(name) {
			text = " "
		}
		return text, true
	}
	return "", false
}

// addSpaceIfNecessary puts a space between markdown and a neighbour that would
// otherwise touch its delimiter.
func addSpaceIfNecessary(n *html.Node, markdown string) string {
	if prev, ok := prevNodeText(n.PrevSibling); ok {
		if last, size := utf8.DecodeLastRuneInString(prev); size > 0 && !unicode.IsSpace(last) {
			markdown = " " + markdown
		}
	}
	if next, ok := nextNodeText(n.NextSibling); ok {
		if first, size := utf8.DecodeRuneInString(next); size > 0 && !unicode.IsSpace(first) && !unicode.IsPunct(first) {
			markdown += " "
		}
	}
	return markdown
}

func isLineCodeDelimiter(chars []rune) bool {
	return len(chars) >= 3 && chars[0] == '`' && chars[1] == '`' && chars[2] == '`'
}

// trimLeadingSpaces removes spaces from the start of each line, leaving list
// items, indented code and fenced code alone.
func trimLeadingSpaces(text string) string {
	var insideCodeBlock bool
	lines := strings.Split(text, "\n")
	for index := range lines {
		chars := []rune(lines[index])
		if isLineCodeDelimiter(chars) {
			insideCodeBlock = !insideCodeBlock
		}
		if insideCodeBlock {
			continue
		}
		var spaces int
		for i := 0; i < len(chars); i++ {
			if unicode.IsSpace(chars[i]) {
				if chars[i] == '\t' {
					spaces += 4
				} else {
					spaces++
				}
				continue
			}
			if chars[i] == '-' { // a list item
				break
			}
			if spaces >= 4 { // a code block
				break
			}
			chars = chars[i:]
			break
		}
		lines[index] = string(chars)
	}
	return strings.Join(lines, "\n")
}

func trimTrailingSpaces(text string) string {
	parts := strings.Split(text, "\n")
	for i := range parts {
		parts[i] = strings.TrimRightFunc(parts[i], unicode.IsSpace)
	}
	return strings.Join(parts, "\n")
}

func escapeMultiLine(content string) string {
	content = strings.TrimSpace(content)
	content = strings.ReplaceAll(content, "\n", `\`+"\n")
	return multipleNewLinesInLinkRegex.ReplaceAllString(content, "\n\\")
}

func codeFenceOccurrences(fenceChar rune, content string) int {
	longest, run := 0, 0
	for _, char := range content {
		if char == fenceChar {
			run++
			continue
		}
		longest = max(longest, run)
		run = 0
	}
	return max(longest, run)
}

// calculateCodeFence is a fence longer than any run of fenceChar in content,
// and at least three long.
func calculateCodeFence(fenceChar rune, content string) string {
	return strings.Repeat(string(fenceChar), max(codeFenceOccurrences(fenceChar, content)+1, 3))
}

// codeContent is the text of a pre/code without its tags: <br> and <div> start
// a line, and script/style/textarea are dropped.
func codeContent(n *html.Node) string {
	var buf bytes.Buffer
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "style" || n.Data == "script" || n.Data == "textarea") {
			return
		}
		if n.Type == html.ElementNode && (n.Data == "br" || n.Data == "div") {
			buf.WriteString("\n")
		}
		if n.Type == html.TextNode {
			buf.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return buf.String()
}

func delimiterForEveryLine(text, delimiter string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines[i] = delimiter + line + delimiter
	}
	return strings.Join(lines, "\n")
}

// isWrapperListItem: an <li> with no text of its own that only wraps a list.
func isWrapperListItem(n *html.Node) bool {
	var direct strings.Builder
	childIsList := false
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if isElement(c, "ul", "ol") {
			childIsList = true
			continue
		}
		direct.WriteString(collectText(c))
	}
	return strings.TrimSpace(direct.String()) == "" && childIsList
}

// listStart is an <ol>'s start attribute, or 1.
func listStart(parent *html.Node) int {
	val, _ := getAttr(parent, "start")
	if val == "" {
		return 1
	}
	num, err := strconv.Atoi(val)
	if err != nil || num < 0 {
		return 1
	}
	return num
}

// listPrefix is an <li>'s marker: "- ", or "1. " padded to the widest number
// in its list ("01. ").
func listPrefix(n *html.Node) string {
	if isWrapperListItem(n) {
		return ""
	}
	parent := parentElement(n)
	switch {
	case isElement(parent, "ul"):
		return bulletListMarker + " "
	case isElement(parent, "ol"):
		currentIndex := listStart(parent) + elementIndex(n)
		lastIndex := elementIndex(lastElementChild(parent)) + 1
		maxLength := len(strconv.Itoa(lastIndex))
		return fmt.Sprintf(`%0`+strconv.Itoa(maxLength)+`d. `, currentIndex)
	}
	return "" // an <li> outside a list
}

// countListParents is the space reserved for the markers of n's own list
// (prefixCount) and of every list above it (previousPrefixCounts).
func countListParents(n *html.Node) (prefixCount, previousPrefixCounts int) {
	first := true
	for p := parentElement(n); p != nil; p = parentElement(p) {
		if isElement(p, "li") {
			continue
		}
		if !isElement(p, "ul", "ol") {
			break
		}
		var width int
		if c := firstElementChild(p); c != nil {
			prefix, _ := getAttr(c, attrListPrefix)
			width = len(prefix)
		}
		if first {
			prefixCount, first = width, false
		} else {
			previousPrefixCounts += width
		}
	}
	return prefixCount, previousPrefixCounts
}

// indentMultiLineListItem indents a list item's continuation lines, up to the
// first line that is itself a list item.
func indentMultiLineListItem(text string, spaces int) string {
	parts := strings.Split(text, "\n")
	for i := range parts {
		if i == 0 {
			continue // indented by the marker
		}
		if isListItem(parts[i]) {
			return strings.Join(parts, "\n")
		}
		parts[i] = strings.Repeat(" ", spaces) + parts[i]
	}
	return strings.Join(parts, "\n")
}

// isListItem reports whether line starts like a Markdown list item.
func isListItem(line string) bool {
	b := []rune(line)
	bulletMarker := []rune(bulletListMarker)[0]
	var hasNumber, hasMarker, hasSpace bool
	for i := 0; i < len(b); i++ {
		if hasMarker && hasSpace {
			if b[i] == bulletMarker {
				return false // another marker: probably a horizontal rule
			}
			if !unicode.IsSpace(b[i]) {
				return true
			}
		}
		if hasMarker {
			if unicode.IsSpace(b[i]) {
				hasSpace = true
				continue
			}
			return false // "1." not followed by a space
		}
		if b[i] == bulletMarker {
			hasMarker = true
			continue
		}
		if hasNumber && b[i] == '.' {
			hasMarker = true
			continue
		}
		if unicode.IsDigit(b[i]) {
			hasNumber = true
			continue
		}
		if unicode.IsSpace(b[i]) {
			continue
		}
		return false
	}
	return false
}

// indexWithText is goquery's Index counting only preceding element siblings
// that have text.
func indexWithText(n *html.Node) int {
	count := 0
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type == html.ElementNode && strings.TrimSpace(collectText(s)) != "" {
			count++
		}
	}
	return count
}
