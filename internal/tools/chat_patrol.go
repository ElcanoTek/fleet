package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// ChatPatrolToolName is the canonical name of the interactive in-chat patrol
// and timer tool.
const ChatPatrolToolName = "chat_patrol"

// Supported actions on chat_patrol.
const (
	ChatPatrolActionCreate = "create"
	ChatPatrolActionList   = "list"
	ChatPatrolActionUpdate = "update"
	ChatPatrolActionPause  = "pause"
	ChatPatrolActionResume = "resume"
	ChatPatrolActionCancel = "cancel"
)

// ChatPatrolParams is the schema-generating input struct for chat_patrol.
type ChatPatrolParams struct {
	Action          string `json:"action" description:"Action to perform in this chat conversation: 'create' (start a recurring cron patrol or one-shot timer in this chat), 'list' (inspect active/paused schedules in this chat), 'update' (change cadence, prompt, or title), 'pause' (temporarily pause), 'resume' (resume a paused patrol), or 'cancel' (stop and remove)."`
	ScheduleID      string `json:"schedule_id,omitempty" description:"Target schedule ID for 'update', 'pause', 'resume', or 'cancel'. Optional when the conversation has only one active/paused schedule, or pass 'all' to cancel all schedules in this chat."`
	Title           string `json:"title,omitempty" description:"Short human-readable label shown in the chat Patrol bar (e.g. '2h Deal Pacing & Margin Patrol' or '30m Backfill Check')."`
	Prompt          string `json:"prompt,omitempty" description:"Instructions executed in this conversation each time the patrol or timer fires. Write clear, actionable instructions (what data/deals/pages to check, what thresholds or anomalies to look for, what updates to make, and how to summarize findings)."`
	Cron            string `json:"cron,omitempty" description:"Standard 5-field cron expression for a recurring patrol in this chat (e.g. '0 */2 * * *' for every 2 hours, '0 9,13,17 * * 1-5' for 3x daily on weekdays). Minimum interval is 5 minutes. Mutually exclusive with duration_seconds."`
	Timezone        string `json:"timezone,omitempty" description:"IANA timezone for evaluating cron (e.g. 'America/New_York', 'America/Chicago', 'UTC'). Defaults to 'UTC'."`
	DurationSeconds int    `json:"duration_seconds,omitempty" description:"Seconds to wait before firing a one-shot timer in this chat (minimum 10, e.g. 1800 for 30 minutes, 7200 for 2 hours). Mutually exclusive with cron."`
	MaxRuns         *int   `json:"max_runs,omitempty" description:"Optional cap on how many times a recurring cron patrol should fire before completing (0 = unlimited)."`
	AllowMultiple   bool   `json:"allow_multiple,omitempty" description:"When true on 'create', adds an additional recurring patrol alongside any existing recurring patrol in this conversation instead of updating the existing one in place."`
}

// ChatPatrolScheduleView is the JSON-serializable view of a conversation schedule
// returned to the model by chat_patrol.
type ChatPatrolScheduleView struct {
	ID              string `json:"id"`
	ConversationID  string `json:"conversation_id"`
	Title           string `json:"title"`
	Prompt          string `json:"prompt"`
	Kind            string `json:"kind"` // "patrol" or "timer"
	Cron            string `json:"cron,omitempty"`
	Timezone        string `json:"timezone"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Status          string `json:"status"`
	NextRunAt       int64  `json:"next_run_at"`
	NextRunISO      string `json:"next_run_iso,omitempty"`
	LastRunAt       *int64 `json:"last_run_at,omitempty"`
	RunCount        int    `json:"run_count"`
	MaxRuns         int    `json:"max_runs,omitempty"`
}

// ChatPatrolResult is the structured result returned by a ChatPatrolHandler.
type ChatPatrolResult struct {
	Status      string                   `json:"status"`
	Action      string                   `json:"action"`
	Message     string                   `json:"message"`
	AutoApprove bool                     `json:"auto_approve"`
	Schedule    *ChatPatrolScheduleView  `json:"schedule,omitempty"`
	Schedules   []ChatPatrolScheduleView `json:"schedules,omitempty"`
}

// ChatPatrolHandler executes chat_patrol actions against the conversation store
// and live SSE turn buffer. Injected onto the turn context by httpapi.
type ChatPatrolHandler func(ctx context.Context, params ChatPatrolParams) (*ChatPatrolResult, error)

type chatPatrolHandlerKey struct{}

// WithChatPatrolHandler attaches a per-turn ChatPatrolHandler to ctx.
func WithChatPatrolHandler(ctx context.Context, h ChatPatrolHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, chatPatrolHandlerKey{}, h)
}

func chatPatrolHandlerFromContext(ctx context.Context) ChatPatrolHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(chatPatrolHandlerKey{}).(ChatPatrolHandler)
	return h
}

const chatPatrolDescription = `Manage in-chat recurring patrols and one-shot timers for THIS conversation.

Unlike schedule_task (which creates a standalone headless job in the Operations Center), chat_patrol schedules recurring cron turns or one-shot wakeups directly inside the current chat conversation. Each time a patrol or timer fires, you wake up in this same conversation with full access to prior chat history, workspace files, persistent Python state, and enabled MCP connectors.

Use chat_patrol when a user asks you to:
- Patrol deals, campaigns, pacing, margin, or pages every N hours in chat ("patrol this every 2 hours", "keep an eye on these deals every 4 hours and update the page")
- Check back after a delay in the same conversation ("check back in 30 minutes", "wake up in 2 hours and verify the backfill")
- View, pause, resume, update, or stop an active patrol/timer in this chat

Best practices:
1. Because chat_patrol executes immediately without ending your turn, when a user asks you to start a patrol AND do an initial check, call chat_patrol(action="create", ...) to arm the recurring schedule and also perform the initial check in the same turn.
2. If auto_approve is false in the tool result and the patrol will invoke critical tools while the user is away, remind the user they can click "⚡ Enable Auto-approve" in the Patrol bar or composer so unattended runs execute without waiting on manual approval cards.`

// NewChatPatrolTool constructs the interactive chat_patrol tool.
func NewChatPatrolTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ChatPatrolToolName,
		chatPatrolDescription,
		func(ctx context.Context, params ChatPatrolParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			action := normalizeChatPatrolAction(params.Action)
			if action == "" {
				return fantasy.NewTextErrorResponse(
					"invalid action: must be one of 'create', 'list', 'update', 'pause', 'resume', or 'cancel'",
				), nil
			}
			params.Action = action

			h := chatPatrolHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse(
					"chat_patrol is only available in interactive chat conversations",
				), nil
			}

			res, err := h(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if out, mErr := json.MarshalIndent(res, "", "  "); mErr == nil {
				return fantasy.NewTextResponse(string(out)), nil
			}
			return fantasy.NewTextResponse(res.Message), nil
		},
	)
}

func normalizeChatPatrolAction(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "create", "start", "set", "schedule":
		return ChatPatrolActionCreate
	case "list", "get", "status", "view":
		return ChatPatrolActionList
	case "update", "edit", "modify":
		return ChatPatrolActionUpdate
	case "pause", "hold":
		return ChatPatrolActionPause
	case "resume", "unpause":
		return ChatPatrolActionResume
	case "cancel", "stop", "delete", "remove":
		return ChatPatrolActionCancel
	default:
		return ""
	}
}

var _ = fmt.Sprintf
