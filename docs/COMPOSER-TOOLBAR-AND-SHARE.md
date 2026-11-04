# Composer Toolbar Declutter & Share From the Start

## What shipped

1. **Calm, focused composer toolbar (mobile & desktop)**
   - **Left cluster (everyday actions):** **Model selector** (`Model`), **Attach files**, **Tools** (connectors with live count badge), and **More options** (`•••`).
   - **More options (`•••`) menu (`composer-more-popover`):** A clean, labeled popover menu with descriptions and status badges for secondary and session-level controls:
     - **Prompt library** — opens the Prompt Library modal and restores focus to **More options** when closed.
     - **Persona** — selects the workspace or personal persona on a new draft before the conversation is created (by sending the first message or materializing via **Share**).
     - **Patrol & schedule** — opens the recurring patrol / one-shot timer popover.
     - **Share this chat** — opens the Share dialog to share with your team, copy a public link, or toggle Multiplayer mode.
     - **Auto-approve tools** — toggles per-conversation auto-approve for session-approvable tools.
     - **Compact conversation** — summarizes earlier turns to free context window space once a conversation has messages.
     - **Send on Enter** — toggles `Enter` vs `Ctrl/Cmd+Enter` send behavior.
   - **Contextual status pills on the primary bar (desktop `sm:` and above; mobile accent dot):** Secondary settings stay out of the way when at their defaults. When any secondary setting is active, **More options** (`•••`) displays an accent dot on both mobile and desktop; on desktop viewports (`sm:` and wider), compact one-click status pills also surface directly on the toolbar:
     - Non-default **Persona** pill (on a new draft before the conversation is created; hidden below `sm` while closed, rendered when opened from **More options**)
     - Active **Patrol** count pill (`N` active schedule(s); hidden below `sm` while closed, rendered when opened from **More options**)
     - Active **Share / Multiplayer** status indicator (`composer-share-button`, `hidden sm:inline-flex`) when a conversation is shared with your team, shared by public link, or in Multiplayer mode
     - Active **Auto-approve** (`⚡`) toggle badge (`hidden sm:inline-flex`)
     - **Context ring** (and the `"Token limit hit — you should compact"` status toast) once context usage exists. Below `sm` the ring shows only when it needs attention (severity `warn`/`danger`, compacting, or the toast); otherwise **Compact conversation** in **More options** carries the percentage (dashboard-view follow-up, docs/DASHBOARD-VIEW.md).

2. **Share in More options (`•••`) from the start of a chat, with an active status indicator when shared**
   - **More options (`•••`) menu & active indicator:** **Share this chat** (`composer-share-button`) lives inside **More options** (`•••`) from the moment a new chat opens — before or after the first message is sent — keeping the high-traffic area next to **Send / Stop** uncluttered when a chat is in its default private (`Only you`) state.
   - **Live state indicator when shared:** Once a conversation is shared with your team, shared by public link, or in Multiplayer mode (`isSharedAny`), **More options** (`•••`) lights up its accent dot and (on `sm:` viewports and wider) a compact icon-only status button (`composer-share-button`) appears in the composer status cluster:
     - accent-tinted people glyph when shared read-only with your team; accent-tinted chain link for a public link (ADR-0057's two audience glyphs)
     - people glyph with an accent dot when Multiplayer mode is active — including on new chats when `FLEET_TEAM_CHATS_SHARED_BY_DEFAULT=true` and the user belongs to a team
   - **Share-first draft materialization (`openShareFromComposer`):** Clicking **Share this chat** on an empty new chat before sending a message materializes the conversation via `POST /api/conversations` while preserving all pre-chat composer state:
     - Draft text and pending attachments (`promoteComposerKey`)
     - Selected persona and model
     - Lockdown state (`pendingLockdown` / `lockdownOnly`)
     - Pre-chat MCP connector toggles (`enabled_optional`) and credential-seat overrides (`mcp_accounts`)
     - Pending `auto_approve` selection
     - Initial title derived from the current draft prompt (or upgraded automatically via `agent.HeuristicTitle` on the first message turn when created with `"New conversation"`)
   - **Top header deduplication:** `ChatShareControls` in the top conversation header keeps the compact visibility status chip (`Only you`, `Multiplayer`, `Shared with your team`, `Public link`) while hiding its duplicate `Share` button (`hideShareButton`).

## Deviations & deferred scope

- **Mobile inline status pills intentionally omitted below `sm`:** Showing passive inline status pills for non-default Persona, active Patrol schedules, and Auto-approve (`⚡`) alongside the primary controls (`Model`, `Attach`, `Tools`, `More options`, `Share`, and `Send`) on narrow `<640px` phone screens caused horizontal crowding against the `Share` and `Send` buttons. On phones (`<sm`), active state for those three settings is instead represented by the accent indicator dot on **More options** (`•••`) and the status badges inside the **More options** menu (while the Persona and Patrol popovers still anchor and open cleanly on mobile when selected from **More options**). Inline one-click status pills on the primary bar are scoped to `sm:` (`>=640px`) viewports.
