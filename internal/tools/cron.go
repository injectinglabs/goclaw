package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// looksLikeLiteralReminder returns true when a cron `message` reads like a
// short reminder the user wants delivered verbatim (take vitamins, brush
// teeth, call mom) rather than an instruction to run at fire time (fetch
// weather, summarise news, search for X). Used to auto-default
// stateless=true for both one-shot and recurring cron jobs — most reminders
// don't need an LLM re-run, and without this models routinely expand a
// two-word reminder into a paragraph.
//
// Heuristic rationale: imperative/verbless short texts are reminders;
// anything containing a "go fetch/compute/look up" keyword is a task.
// Keyword set covers English + Russian since those are the primary
// languages agents receive from users here.
func looksLikeLiteralReminder(message string) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	if m == "" || len(m) > 200 {
		return false
	}
	tasky := []string{
		// English
		"fetch", "retrieve", "look up", "lookup", "search for",
		"calculate", "summarize", "summarise", "analyze", "analyse",
		"generate ", "compose ", "draft ", "write me ",
		"check the ", "check my ", "review the ", "review my ",
		// Russian
		"найди ", "найти ", "получи ", "получить ", "посмотри ",
		"проверь ", "подсчитай ", "проанализируй ", "собери ",
		"скачай ", "покажи мне ", "выведи ", "напиши мне ",
	}
	for _, k := range tasky {
		if strings.Contains(m, k) {
			return false
		}
	}
	return true
}

// CronTool lets agents manage Gateway cron jobs.
// Matching OpenClaw src/agents/tools/cron-tool.ts.
type CronTool struct {
	cronStore store.CronStore
	permStore store.ConfigPermissionStore // nil = no group restriction
}

func NewCronTool(cronStore store.CronStore) *CronTool {
	return &CronTool{cronStore: cronStore}
}

// SetConfigPermStore enables group cron mutation restriction.
func (t *CronTool) SetConfigPermStore(s store.ConfigPermissionStore) {
	t.permStore = s
}

func (t *CronTool) Name() string { return "cron" }

func (t *CronTool) Description() string {
	return `Manage Gateway cron jobs.
Always send a JSON object with an "action" field.

VALID ACTIONS AND EXACT PAYLOAD SHAPES:
1) status
{ "action": "status" }

2) list
{ "action": "list", "includeDisabled": true|false }

3) add
{
  "action": "add",
  "job": {
    "name": "string",             // required, lowercase slug: [a-z0-9-]+
    "schedule": { ... },          // required
    "message": "string",          // required
    "deliver": true|false,        // set true for reminders / proactive messages. Auto-defaults to true for one-shot at-jobs with a non-empty message.
    "channel": "string",          // channel_instance name (see ## Connected Channels in system prompt). Auto-falls back to the current session channel if you omit it. When the user explicitly asks "in Telegram/Slack/...", pass that channel name.
    "to": "string",               // deliver_to (chat_id). For DM bots pick the value from ## Connected Channels; for ws/browser pass current session key.
    "stateless": true|false,      // Auto-defaults to true for literal reminders: short messages with no fetch/look-up/compute keywords ("заварить чай", "принять витамины", "call mom"). Works for all schedule kinds — one-shot and recurring alike skip the fire-time LLM turn and broadcast the literal message. Explicitly set false when the job must compute at fire time — look up data, call tools, generate fresh content ("fetch weather forecast", "summarize today's unread emails").
    "agentId": "string",          // optional, defaults to current agent
    "deleteAfterRun": true|false  // optional, default true for schedule.kind="at"
  }
}

4) update
{
  "action": "update",
  "jobId": "string",
  "patch": {
    "name": "string",
    "schedule": { ... },
    "message": "string",
    "deliver": true|false,
    "channel": "string",
    "to": "string",
    "agentId": "string",
    "deleteAfterRun": true|false,
    "disabled": true|false
  }
}

5) remove
{ "action": "remove", "jobId": "string" }

6) run
{ "action": "run", "jobId": "string" }
// Force-triggers a job right now. Use ONLY when the user explicitly asks
// to run/test a specific job. Do NOT call this to "check" whether a
// scheduled job is working — scheduled fires happen automatically; use
// action="runs" to inspect recent outcomes. If a manual run returns
// "already running", the job is fine — a previous fire is still in
// flight. Never delete a job just because a run probe hit that state.

7) runs
{ "action": "runs", "jobId": "string" }

SCHEDULE SCHEMA:
- at: { "kind": "at", "atMs": <unix-milliseconds> }
- every: { "kind": "every", "everyMs": <interval-ms> }
- cron: { "kind": "cron", "expr": "<5-field cron>", "tz": "<IANA timezone, e.g. Asia/Ho_Chi_Minh; omit for gateway default>" }

RULES:
- For action="add", send the job inside "job". Do not place job fields at the root level.
- For action="update", send changes inside "patch". Do not place patch fields at the root level.
- Always use "jobId". Do not use "id".
- "name", "schedule", and "message" are required for add.
- "name" must match: lowercase letters, numbers, hyphens only.
- Before creating or updating a scheduled job, call the datetime tool first to get the precise current time and unix_ms timestamp. Never guess timestamps.
- Omit optional fields when unknown; do not invent placeholder values like "", 0, or null unless required.
- Jobs run as isolated agent turns using the provided "message".`
}

func (t *CronTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "The cron action to perform",
				"enum":        []string{"status", "list", "add", "update", "remove", "run", "runs"},
			},
			"includeDisabled": map[string]any{
				"type":        "boolean",
				"description": "Include disabled jobs in list (default false)",
			},
			"job": map[string]any{
				"type":                 "object",
				"description":          "Job definition for add action (name, schedule, message, deliver, channel, to, agentId, deleteAfterRun)",
				"additionalProperties": true,
			},
			"jobId": map[string]any{
				"type":        "string",
				"description": "Job ID for update/remove/run/runs actions",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Backward compatibility alias for jobId",
			},
			"patch": map[string]any{
				"type":                 "object",
				"description":          "Patch object for update action",
				"additionalProperties": true,
			},
			"runMode": map[string]any{
				"type":        "string",
				"description": "Run mode: 'due' (only if due) or 'force' (immediate)",
				"enum":        []string{"due", "force"},
			},
		},
		"required": []string{"action"},
	}
}

func (t *CronTool) Execute(ctx context.Context, args map[string]any) *Result {
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action parameter is required")
	}

	// Group cron permission check for mutation actions
	if t.permStore != nil && (action == "add" || action == "update" || action == "remove") {
		if err := store.CheckCronPermission(ctx, t.permStore); err != nil {
			return ErrorResult("permission denied: only users with cron or file_writer permission can manage cron jobs in group chats")
		}
	}

	agentID := resolveAgentIDString(ctx)
	// SCOPE-intentional (#915 audit 2026-04-16): cron jobs follow the per-group
	// memory model — all group members share the same cron_jobs rows via the
	// group-scope user_id. Migrating to ActorIDFromContext would split jobs
	// per-individual and break collaborative /cron list/add/remove UX.
	userID := store.UserIDFromContext(ctx)

	switch action {
	case "status":
		return t.handleStatus()
	case "list":
		return t.handleList(ctx, args, agentID, userID)
	case "add":
		return t.handleAdd(ctx, args, agentID, userID)
	case "update":
		return t.handleUpdate(ctx, args, agentID, userID)
	case "remove":
		return t.handleRemove(ctx, args, agentID, userID)
	case "run":
		return t.handleRun(ctx, args, agentID, userID)
	case "runs":
		return t.handleRuns(ctx, args, agentID, userID)
	default:
		return ErrorResult(fmt.Sprintf("unknown action: %s", action))
	}
}

func (t *CronTool) handleStatus() *Result {
	status := t.cronStore.Status()
	data, _ := json.MarshalIndent(status, "", "  ")
	return NewResult(string(data))
}

func (t *CronTool) handleList(ctx context.Context, args map[string]any, agentID, userID string) *Result {
	includeDisabled, _ := args["includeDisabled"].(bool)
	jobs := t.cronStore.ListJobs(ctx, includeDisabled, agentID, userID)

	result := map[string]any{
		"jobs":  jobs,
		"count": len(jobs),
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return NewResult(string(data))
}

func (t *CronTool) handleAdd(ctx context.Context, args map[string]any, agentID, userID string) *Result {
	jobObj, ok := args["job"].(map[string]any)
	if !ok {
		return ErrorResult("job object is required for add action")
	}

	name, _ := jobObj["name"].(string)
	if name == "" {
		return ErrorResult("job.name is required")
	}

	scheduleObj, ok := jobObj["schedule"].(map[string]any)
	if !ok {
		return ErrorResult("job.schedule is required")
	}

	message, _ := jobObj["message"].(string)
	if message == "" {
		return ErrorResult("job.message is required")
	}

	// Parse schedule
	schedule := store.CronSchedule{
		Kind: stringFromMap(scheduleObj, "kind"),
	}
	if schedule.Kind == "" {
		return ErrorResult("job.schedule.kind is required (at, every, or cron)")
	}

	switch schedule.Kind {
	case "at":
		if v, ok := numberFromMap(scheduleObj, "atMs"); ok {
			ms := int64(v)
			if ms <= time.Now().UnixMilli() {
				return ErrorResult(fmt.Sprintf("job.schedule.atMs is in the past (%d). Use a future Unix timestamp in milliseconds. Current time is %d ms", ms, time.Now().UnixMilli()))
			}
			schedule.AtMS = &ms
		} else {
			return ErrorResult("job.schedule.atMs is required for 'at' schedule")
		}
	case "every":
		if v, ok := numberFromMap(scheduleObj, "everyMs"); ok {
			ms := int64(v)
			schedule.EveryMS = &ms
		} else {
			return ErrorResult("job.schedule.everyMs is required for 'every' schedule")
		}
	case "cron":
		schedule.Expr = stringFromMap(scheduleObj, "expr")
		if schedule.Expr == "" {
			return ErrorResult("job.schedule.expr is required for 'cron' schedule")
		}
		schedule.TZ = stringFromMap(scheduleObj, "tz")
		if schedule.TZ != "" {
			if _, err := time.LoadLocation(schedule.TZ); err != nil {
				return ErrorResult(fmt.Sprintf("invalid timezone '%s': use IANA names like 'Asia/Ho_Chi_Minh', 'America/New_York'", schedule.TZ))
			}
		}
	default:
		return ErrorResult(fmt.Sprintf("invalid schedule kind: %s (must be at, every, or cron)", schedule.Kind))
	}

	// Optional fields
	deliver, _ := jobObj["deliver"].(bool)
	channel, _ := jobObj["channel"].(string)
	to, _ := jobObj["to"].(string)

	// Resolve `stateless` with auto-default for reminder-shaped jobs.
	// User intent "напомни мне X" → cron fires and the user should see the
	// short reminder text. Without stateless, cron starts a fresh agent turn
	// with `message` as a prompt, and the LLM expands it (e.g. "brew tea" →
	// full brewing recipe, "принять витамины" → article on vitamins). LLMs
	// usually don't set stateless on create.
	//
	// Auto-default stateless=true whenever the message looks like a literal
	// reminder (short, no "fetch / look up / summarise / ..." keywords) —
	// independent of schedule kind, so daily/weekly vitamins reminders work
	// the same way as one-shot "in 20s" reminders.
	//
	// Explicit override wins: {"stateless":false} for reminders that really
	// do need an agent run ("tomorrow 18:00 fetch the weather forecast").
	statelessExplicit, statelessWasSet := jobObj["stateless"].(bool)
	var stateless bool
	if statelessWasSet {
		stateless = statelessExplicit
	} else if looksLikeLiteralReminder(message) {
		stateless = true
	}

	// Safety net for the common reminder use-case: one-shot at-job with
	// a non-empty message almost always wants deliver=true. Models drop this
	// flag often — flip it on when it wasn't explicitly set false.
	if _, deliverExplicit := jobObj["deliver"]; !deliverExplicit && schedule.Kind == "at" && message != "" {
		deliver = true
	}

	// Auto-default deliver=true when the request comes from a real channel
	// (not CLI/system/subagent). Users chatting on Zalo/Telegram expect
	// cron results delivered back to the same chat.
	if !deliver {
		if ctxChannel := ToolChannelFromCtx(ctx); ctxChannel != "" {
			switch ctxChannel {
			case "cli", "system", "subagent", "cron", "teammate":
				// internal channels — don't auto-deliver
			default:
				deliver = true
			}
		}
	}

	// Fill channel/to from the current session context ONLY when the agent
	// did not supply them explicitly. Previously we always overrode the
	// agent's values to protect against misrouting (LLM confusing guild ID
	// with channel ID), but that also blocked legitimate cross-channel
	// reminders like "in extension: remind me in Telegram in 10s" — the
	// agent's channel="telegram-<sub>" was silently rewritten to the current
	// ws session. The Connected Channels prompt section already enumerates
	// the tenant's real channel_instance names; trusting an agent-supplied
	// value is safer than the dispatcher-level unknown-channel drop that
	// would result from an LLM hallucination.
	if deliver {
		if channel == "" {
			if ctxChannel := ToolChannelFromCtx(ctx); ctxChannel != "" {
				channel = ctxChannel
			}
		}
		if to == "" {
			// For internal channels (ws/browser) there's no external chat_id —
			// the WS chat.send frame populates RunRequest.ChatID with the
			// user_id for workspace scoping, not routing. What cron delivery
			// actually needs as DeliverTo is the originating session_key so:
			//   - AddMessage can write the reply into the right chat's history
			//   - the broadcast's session_key maps to a real client chat
			//   - the reminders row's origin_session_key is actionable for jump
			if channel == "ws" || channel == "browser" {
				if sk := ToolSessionKeyFromCtx(ctx); sk != "" {
					to = sk
				}
			}
			if to == "" {
				if ctxChatID := ToolChatIDFromCtx(ctx); ctxChatID != "" {
					to = ctxChatID
				}
			}
		}
	}

	// Use agent ID from job object if explicitly provided, otherwise from context
	if explicit, _ := jobObj["agentId"].(string); explicit != "" {
		agentID = explicit
	}

	job, err := t.cronStore.AddJob(ctx, name, schedule, message, deliver, channel, to, agentID, userID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to create cron job: %v", err))
	}

	// Set wake_heartbeat if requested (triggers heartbeat after cron job completes)
	if wh, _ := jobObj["wake_heartbeat"].(bool); wh {
		wakeTrue := true
		if updated, uErr := t.cronStore.UpdateJob(ctx, job.ID, store.CronJobPatch{WakeHeartbeat: &wakeTrue}); uErr == nil {
			job = updated
		}
	}

	// Apply `stateless` via patch. AddJob doesn't thread the flag through
	// (historical reason — store/cron_store.go:111 signature predates it),
	// so reproduce the same pattern used for wake_heartbeat above.
	if stateless {
		statelessVal := true
		if updated, uErr := t.cronStore.UpdateJob(ctx, job.ID, store.CronJobPatch{Stateless: &statelessVal}); uErr == nil {
			job = updated
		}
	}



	data, _ := json.MarshalIndent(map[string]any{"job": job}, "", "  ")
	return NewResult(string(data))
}

// checkJobOwnership validates that the job belongs to the current agent+user scope.
// When agentID/userID is empty, all jobs are accessible.
func (t *CronTool) checkJobOwnership(ctx context.Context, jobID, agentID, userID string) (*store.CronJob, *Result) {
	job, ok := t.cronStore.GetJob(ctx, jobID)
	if !ok {
		return nil, ErrorResult(fmt.Sprintf("job %s not found", jobID))
	}

	// Verify ownership
	if agentID != "" && job.AgentID != agentID {
		return nil, ErrorResult(fmt.Sprintf("job %s not found", jobID))
	}
	if userID != "" && job.UserID != userID {
		return nil, ErrorResult(fmt.Sprintf("job %s not found", jobID))
	}

	return job, nil
}

func (t *CronTool) handleUpdate(ctx context.Context, args map[string]any, agentID, userID string) *Result {
	jobID := resolveJobID(args)
	if jobID == "" {
		return ErrorResult("jobId is required for update action")
	}

	if _, errResult := t.checkJobOwnership(ctx, jobID, agentID, userID); errResult != nil {
		return errResult
	}

	patchObj, ok := args["patch"].(map[string]any)
	if !ok {
		return ErrorResult("patch object is required for update action")
	}

	var patch store.CronJobPatch
	// Re-marshal and unmarshal to leverage JSON tags
	patchJSON, _ := json.Marshal(patchObj)
	json.Unmarshal(patchJSON, &patch)

	// Validate atMs not in the past when updating schedule
	if patch.Schedule != nil && patch.Schedule.Kind == "at" && patch.Schedule.AtMS != nil {
		if *patch.Schedule.AtMS <= time.Now().UnixMilli() {
			return ErrorResult(fmt.Sprintf("schedule.atMs is in the past (%d). Use the datetime tool to get current time, then set a future timestamp. Current time is %d ms", *patch.Schedule.AtMS, time.Now().UnixMilli()))
		}
	}

	job, err := t.cronStore.UpdateJob(ctx, jobID, patch)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to update cron job: %v", err))
	}

	data, _ := json.MarshalIndent(map[string]any{"job": job}, "", "  ")
	return NewResult(string(data))
}

func (t *CronTool) handleRemove(ctx context.Context, args map[string]any, agentID, userID string) *Result {
	jobID := resolveJobID(args)
	if jobID == "" {
		return ErrorResult("jobId is required for remove action")
	}

	if _, errResult := t.checkJobOwnership(ctx, jobID, agentID, userID); errResult != nil {
		return errResult
	}

	if err := t.cronStore.RemoveJob(ctx, jobID); err != nil {
		return ErrorResult(fmt.Sprintf("failed to remove cron job: %v", err))
	}

	data, _ := json.MarshalIndent(map[string]any{"deleted": true, "jobId": jobID}, "", "  ")
	return NewResult(string(data))
}

func (t *CronTool) handleRun(ctx context.Context, args map[string]any, agentID, userID string) *Result {
	jobID := resolveJobID(args)
	if jobID == "" {
		return ErrorResult("jobId is required for run action")
	}

	if _, errResult := t.checkJobOwnership(ctx, jobID, agentID, userID); errResult != nil {
		return errResult
	}

	runMode, _ := args["runMode"].(string)
	force := runMode == "force"

	ran, reason, err := t.cronStore.RunJob(ctx, jobID, force)
	if err != nil {
		return ErrorResult(fmt.Sprintf("failed to run cron job: %v", err))
	}

	result := map[string]any{
		"ran":   ran,
		"jobId": jobID,
	}
	if !ran && reason != "" {
		result["reason"] = reason
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return NewResult(string(data))
}

func (t *CronTool) handleRuns(ctx context.Context, args map[string]any, agentID, userID string) *Result {
	jobID := resolveJobID(args)

	// Validate ownership if a specific job is requested
	if jobID != "" {
		if _, errResult := t.checkJobOwnership(ctx, jobID, agentID, userID); errResult != nil {
			return errResult
		}
	}

	limit := 20
	if v, ok := numberFromMap(args, "limit"); ok {
		limit = int(v)
	}

	entries, total := t.cronStore.GetRunLog(ctx, jobID, limit, 0)

	result := map[string]any{
		"entries": entries,
		"count":   len(entries),
		"total":   total,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return NewResult(string(data))
}

// --- helpers ---

func resolveJobID(args map[string]any) string {
	if id, ok := args["jobId"].(string); ok && id != "" {
		return id
	}
	if id, ok := args["id"].(string); ok && id != "" {
		return id
	}
	return ""
}

func stringFromMap(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func numberFromMap(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}
