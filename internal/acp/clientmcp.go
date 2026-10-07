package acp

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	acpsdk "github.com/coder/acp-go-sdk"
)

// The MCP servers an ACP client sends with session/new are accepted and
// ignored (see Agent.NewSession). What fleet acp keeps of them is this file's
// business: the names, made safe to show, and how many there were. An entry
// can carry secrets in its env values, HTTP headers, args, URL (a query
// string) or _meta, so nothing but the name is ever used.

const (
	// maxListedMCPServers caps how many names the notice and the stderr line
	// list; the rest are counted ("and 3 more").
	maxListedMCPServers = 5
	// maxMCPNameRunes caps one listed name, cut with "…" beyond it.
	maxMCPNameRunes = 60
)

// clientMCPServers is the display form of the MCP servers a client sent: the
// first maxListedMCPServers names (displayName) and how many were sent.
type clientMCPServers struct {
	shown []string
	total int
}

// ignoredMCPServers keeps what may be shown of servers, and nothing else: the
// first names and how many there are.
func ignoredMCPServers(servers []acpsdk.McpServer) clientMCPServers {
	c := clientMCPServers{total: len(servers)}
	for _, s := range servers[:min(len(servers), maxListedMCPServers)] {
		c.shown = append(c.shown, displayName(mcpServerName(s)))
	}
	return c
}

// mcpServerName is an entry's name, whichever transport it describes. An
// entry of no variant the SDK knows has none ("").
func mcpServerName(s acpsdk.McpServer) string {
	switch {
	case s.Stdio != nil:
		return s.Stdio.Name
	case s.Http != nil:
		return s.Http.Name
	case s.Sse != nil:
		return s.Sse.Name
	case s.Acp != nil:
		return s.Acp.Name
	}
	return ""
}

// displayName makes a name the client chose safe to show on one line, in the
// client's transcript and on stderr: a line break, tab or other control
// character becomes a space, an invisible format character (a bidi override,
// say) is dropped, runs of space collapse, and the result is cut at
// maxMCPNameRunes. Other characters, invisible ones included (U+3164, a
// variation selector), pass through: inside the notice's inline code and the
// stderr line's quotes they cannot break either. A name left empty stays "":
// the notice says "unnamed", and stderr quotes it as "".
func displayName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r):
			b.WriteRune(' ')
		case unicode.Is(unicode.Cf, r):
			// dropped: it would show nothing, or reorder what follows it
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(s); len(r) > maxMCPNameRunes {
		s = string(r[:maxMCPNameRunes]) + "…"
	}
	return s
}

// list joins the shown names, each passed through show, and counts the ones
// not shown.
func (c clientMCPServers) list(show func(string) string) string {
	names := make([]string, len(c.shown))
	for i, n := range c.shown {
		names[i] = show(n)
	}
	s := strings.Join(names, ", ")
	if more := c.total - len(c.shown); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

// noun is "MCP server" for one server sent, else "MCP servers".
func (c clientMCPServers) noun() string {
	if c.total == 1 {
		return "MCP server"
	}
	return "MCP servers"
}

// notice is what the session's first submitted prompt opens with (see
// session.mcpNotice): an agent message of its own paragraph, ahead of the
// turn's output. Each name is inline code; an empty one is "unnamed" in plain
// text, so it cannot be mistaken for a server of that name.
func (c clientMCPServers) notice() string {
	show := func(name string) string {
		if name == "" {
			return "unnamed"
		}
		return codeSpan(name)
	}
	return fmt.Sprintf("fleet does not use the %s your editor sent (%s): fleet's tools and connectors come from the fleet operator and run on the fleet server.\n\n",
		c.noun(), c.list(show))
}

// stderrLine is what NewSession tells the operator, on stderr. Each name is
// quoted, so one holding ", " or ")" cannot blur where the list ends.
func (c clientMCPServers) stderrLine() string {
	return fmt.Sprintf("fleet acp: ignoring %d %s sent by the client (%s); fleet's connectors come from the operator's bundle\n",
		c.total, c.noun(), c.list(strconv.Quote))
}

// codeSpan shows a name as Markdown inline code, so it reads as written
// rather than as Markdown of its own (a link, emphasis) in a client that
// renders the transcript. The backtick run around it is longer than any run
// inside it, and a name that starts or ends with a backtick is padded with a
// space, which CommonMark strips from a code span.
func codeSpan(s string) string {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}
