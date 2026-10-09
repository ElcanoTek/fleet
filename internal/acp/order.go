package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
)

// orderedInput is fleet acp's stdin as the SDK reads it, so the agent
// handles session/prompt and session/cancel in the order the client sent
// them.
//
// The SDK does not keep that order (acp-go-sdk v0.13.5, Connection.receive):
// it runs each request on its own goroutine but queues notifications, so a
// prompt and a cancel sent close together can reach the agent either way
// round. Seen live from Buzz's buzz-acp, which sends a session/cancel 0.25 ms
// behind a prompt when a new message arrives as that prompt's turn starts:
// Cancel ran first and reached nothing, the turn ran on, and buzz-acp, which
// waits five seconds for a cancelled turn to end, killed fleet acp. The other
// way round, a prompt sent just after a cancel could be tracked before it
// and be stopped with the turn the cancel was for; and two prompts sent back
// to back could take the session in the wrong order.
//
// orderedInput reads the client's lines in order and holds a line back from
// the SDK until the agent has caught up with those before it (awaitOrder): a
// session/cancel until every session/prompt sent before it has reached
// Prompt, where it is tracked, so Cancel finds it; a session/prompt until
// every earlier prompt has too, and every earlier cancel has been handled.
// Every other line passes straight through. The wait is normally
// microseconds: nothing between the SDK's read and either point does I/O.
//
// Only lines the SDK delivers are counted (sdkDelivers), so a counted
// message is only ever late, never lost. One can be late for long: a
// session/prompt a client sends as a notification has its whole turn run on
// the SDK's single notification goroutine, and a cancel queued behind it is
// handled only after that turn. So the wait is bounded by orderWait: a line
// that reaches the bound is handed on anyway, and the wait says so on
// stderr. The counts stay exact, so the late message is still expected, and
// lines after it keep their order once it arrives.
type orderedInput struct {
	r     *bufio.Reader
	agent *Agent
	// prompts and cancels count the session/prompt and session/cancel lines
	// (requests and notifications alike) handed to the SDK.
	prompts, cancels uint64
	pending          []byte // the rest of the current line, not yet read by the SDK
	long             bool   // the current line is over maxLine: passed on as it comes
	err              error
}

// orderWait bounds one wait for the agent to catch up. A var so tests can
// shorten it; well inside the five seconds buzz-acp gives a cancelled turn.
var orderWait = 2 * time.Second

// maxLine is the SDK's own line limit (Connection.receive's scanner). A
// longer line ends the connection there, so orderedInput hands it on without
// decoding it or waiting, and once it has read maxLine bytes of it without
// its end, as the rest comes, instead of holding it in memory.
const maxLine = 10 << 20

func newOrderedInput(r io.Reader, agent *Agent) *orderedInput {
	return &orderedInput{r: bufio.NewReaderSize(r, 64<<10), agent: agent}
}

func (o *orderedInput) Read(p []byte) (int, error) {
	for len(o.pending) == 0 {
		if o.err != nil {
			return 0, o.err
		}
		o.next()
	}
	n := copy(p, o.pending)
	o.pending = o.pending[n:]
	return n, nil
}

// next reads the next line into pending, or the next piece of a line over
// maxLine, holding a line back until the agent has caught up (hold).
func (o *orderedInput) next() {
	if o.long {
		chunk, err := o.r.ReadSlice('\n')
		o.pending = append([]byte(nil), chunk...)
		switch {
		case err == nil:
			o.long = false
		case !errors.Is(err, bufio.ErrBufferFull):
			o.err = err
		}
		return
	}
	var line []byte
	for {
		chunk, err := o.r.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			// At maxLine the SDK's scanner already refuses the line, so hand
			// it on now: a client that stalls there must not hang us.
			if len(line) >= maxLine {
				o.long = true
				o.pending = line
				return
			}
			continue
		}
		if err != nil {
			o.err = err
		}
		break
	}
	if len(line) > 0 && len(bytes.TrimSuffix(line, []byte("\n"))) <= maxLine {
		o.hold(line)
	}
	o.pending = line
}

// hold waits until the agent has caught up with what the client sent before
// line, when line is a session/prompt or session/cancel the SDK will deliver,
// and counts it.
func (o *orderedInput) hold(line []byte) {
	switch sdkDelivers(line) {
	case acpsdk.AgentMethodSessionPrompt:
		o.await(acpsdk.AgentMethodSessionPrompt, o.prompts, o.cancels)
		o.prompts++
	case acpsdk.AgentMethodSessionCancel:
		o.await(acpsdk.AgentMethodSessionCancel, o.prompts, 0)
		o.cancels++
	}
}

// await waits (awaitOrder) for prompts prompts to have arrived and cancels
// cancels to have been handled, for at most orderWait. A wait that reaches
// the bound says so on stderr; the counts are left as they are (see
// orderedInput). The note is written off this goroutine: a stderr nobody
// drains can hold a write for stderrWait, and that must not stretch the
// bound the held line has already reached.
func (o *orderedInput) await(method string, prompts, cancels uint64) {
	arrived, handled, caughtUp := o.agent.awaitOrder(prompts, cancels, orderWait)
	if caughtUp {
		return
	}
	note := fmt.Sprintf("fleet acp: %d session/prompt and %d session/cancel sent before a %s had not reached fleet acp within %s; handing it on without them\n",
		prompts-min(arrived, prompts), cancels-min(handled, cancels), method, orderWait)
	go func() { _, _ = io.WriteString(o.agent.diag, note) }()
}

// sdkDelivers is line's method when line is a session/prompt or
// session/cancel the SDK hands the agent, and "" otherwise. It decodes line
// as the SDK does (Connection.receive's message, then the generated
// dispatch's params and Validate), so it counts exactly what reaches Prompt
// and Cancel; a request (with an id) and a notification both reach them.
func sdkDelivers(line []byte) string {
	if len(bytes.TrimSpace(line)) == 0 {
		return ""
	}
	var msg struct {
		JSONRPC string               `json:"jsonrpc"`
		ID      *json.RawMessage     `json:"id,omitempty"`
		Method  string               `json:"method,omitempty"`
		Params  json.RawMessage      `json:"params,omitempty"`
		Result  json.RawMessage      `json:"result,omitempty"`
		Error   *acpsdk.RequestError `json:"error,omitempty"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return ""
	}
	switch msg.Method {
	case acpsdk.AgentMethodSessionPrompt:
		var p acpsdk.PromptRequest
		if json.Unmarshal(msg.Params, &p) != nil || p.Validate() != nil {
			return ""
		}
	case acpsdk.AgentMethodSessionCancel:
		var c acpsdk.CancelNotification
		if json.Unmarshal(msg.Params, &c) != nil || c.Validate() != nil {
			return ""
		}
	default:
		return ""
	}
	return msg.Method
}
