// Switchboard lifecycle reporting for Pi (https://pi.dev).
//
// Install: symlink this file into Pi's extension directory, so the live
// extension is always the repo's:
//
//   ln -s ~/Projects/switchboard/integrations/pi/switchboard.ts \
//         ~/.pi/agent/extensions/switchboard.ts
//
// Replace any older copy at that path with the link. It sits beside herdr's own
// managed extension and does not touch it: herdr keeps reporting
// working/blocked/idle to herdr, and this reports to the Switchboard daemon
// through `switchboard-ctl pi-hook <event>` (stdin = JSON), the way Claude Code
// and Codex hooks do.
//
// Events, in Claude Code's hook vocabulary so the daemon has one table. Every
// payload also carries session_id, transcript_path and cwd.
//   session_start                       → SessionStart {source, previous_session_file, session_name, busy}
//   session_shutdown                    → SessionEnd {source}
//   agent_start                         → UserPromptSubmit
//   tool_execution_start / _end         → PreToolUse / PostToolUse {tool_name, tool_use_id}
//   ui_prompt_start, herdr:blocked      → PermissionRequest {open_dialogs}
//   ui_prompt_end, herdr:blocked off    → PermissionResolved {open_dialogs}
//   message_end (assistant)             → Usage {token counts, cost_total, provider, model, message_id}
//   agent_settled                       → Stop, or StopFailure {error_message} after an error stop
//
// open_dialogs is the number of dialogs open now (the ui_prompt span plus
// herdr's blocked counter), not an edge, so a lost or reordered hook
// self-corrects on the next one. Pi's built-in pickers (/model, /resume,
// project trust) fire no ui_prompt event and are not counted.
//
// Nothing user-authored is sent except the /name session name: no prompt, no
// dialog title or herdr label, no tool input or output. The one exception is
// a failed run's error text, which switchboard-ctl reduces to a usage-limit
// verdict before anything reaches the daemon.
// @ts-nocheck

import { spawn } from "node:child_process";
import os from "node:os";
import path from "node:path";

const ctl =
  process.env.SWITCHBOARD_CTL ||
  path.join(os.homedir(), ".local/share/switchboard/current/switchboard-ctl");

const maxErrorMessage = 2000;

function send(event: string, payload: Record<string, unknown>): void {
  try {
    const child = spawn(ctl, ["pi-hook", event], { stdio: ["pipe", "ignore", "ignore"] });
    child.on("error", () => {});
    const timer = setTimeout(() => child.kill(), 2000);
    timer.unref?.();
    child.on("exit", () => clearTimeout(timer));
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(payload));
  } catch {
    // A missing or broken switchboard-ctl must never disturb the agent.
  }
}

function sessionFields(ctx: any): Record<string, unknown> {
  const fields: Record<string, unknown> = { cwd: process.cwd() };
  try {
    const id = ctx?.sessionManager?.getSessionId?.();
    if (typeof id === "string" && id) fields.session_id = id;
    const file = ctx?.sessionManager?.getSessionFile?.();
    if (typeof file === "string" && path.isAbsolute(file)) fields.transcript_path = file;
  } catch {}
  return fields;
}

function sessionName(ctx: any): string | undefined {
  try {
    const name = ctx?.sessionManager?.getSessionName?.();
    if (typeof name === "string" && name) return name;
  } catch {}
  return undefined;
}

function count(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? Math.floor(value) : 0;
}

export default function (pi) {
  // Per-session state. A reload replaces this extension instance, so
  // session_start rebuilds all of it.
  let interactive = false;
  let promptOpen = false;
  let herdrBlocked = 0;
  let lastAssistant: { stopReason?: string; errorMessage?: string } | undefined;
  let lastCtx: any;

  function dialogs(event: "PermissionRequest" | "PermissionResolved", ctx: any): void {
    send(event, { ...sessionFields(ctx), open_dialogs: (promptOpen ? 1 : 0) + herdrBlocked });
  }

  pi.on("session_start", (event, ctx) => {
    // RPC/JSON/print runs have no pane to colour; mirror herdr's gate.
    interactive = ctx?.mode === "tui";
    promptOpen = false;
    herdrBlocked = 0;
    lastAssistant = undefined;
    lastCtx = ctx;
    if (!interactive) return;
    const payload: Record<string, unknown> = {
      ...sessionFields(ctx),
      source: event?.reason,
      // A reload can replace this extension mid-run without another agent_start.
      busy: ctx?.isIdle?.() === false,
    };
    const previous = event?.previousSessionFile;
    if (typeof previous === "string" && path.isAbsolute(previous)) payload.previous_session_file = previous;
    const name = sessionName(ctx);
    if (name) payload.session_name = name;
    send("SessionStart", payload);
  });

  pi.on("session_shutdown", (event, ctx) => {
    if (!interactive) return;
    send("SessionEnd", { ...sessionFields(ctx), source: event?.reason });
  });

  pi.on("agent_start", (_event, ctx) => {
    if (!interactive) return;
    lastCtx = ctx;
    lastAssistant = undefined;
    send("UserPromptSubmit", sessionFields(ctx));
  });

  pi.on("tool_execution_start", (event, ctx) => {
    if (!interactive) return;
    send("PreToolUse", { ...sessionFields(ctx), tool_name: event?.toolName, tool_use_id: event?.toolCallId });
  });

  pi.on("tool_execution_end", (event, ctx) => {
    if (!interactive) return;
    send("PostToolUse", { ...sessionFields(ctx), tool_name: event?.toolName, tool_use_id: event?.toolCallId });
  });

  // Nested extension dialogs coalesce into one span; only its kind would be
  // safe to forward, and the count does not need it.
  pi.on("ui_prompt_start", (_event, ctx) => {
    if (!interactive) return;
    promptOpen = true;
    dialogs("PermissionRequest", ctx);
  });

  pi.on("ui_prompt_end", (_event, ctx) => {
    if (!interactive) return;
    promptOpen = false;
    dialogs("PermissionResolved", ctx);
  });

  // Approval-gate extensions announce their own waits on herdr's channel,
  // which carries no ctx; the label is never read.
  pi.events?.on?.("herdr:blocked", (data) => {
    if (!interactive) return;
    if (data?.active) {
      herdrBlocked += 1;
      dialogs("PermissionRequest", lastCtx);
      return;
    }
    herdrBlocked = Math.max(0, herdrBlocked - 1);
    dialogs("PermissionResolved", lastCtx);
  });

  pi.on("message_end", (event, ctx) => {
    const message = event?.message;
    if (message?.role !== "assistant") return;
    lastAssistant = { stopReason: message.stopReason, errorMessage: message.errorMessage };
    if (!interactive) return;
    const usage = message.usage ?? {};
    const payload: Record<string, unknown> = {
      ...sessionFields(ctx),
      provider: message.provider,
      model: message.model,
      input_tokens: count(usage.input),
      output_tokens: count(usage.output),
      cache_read_tokens: count(usage.cacheRead),
      cache_write_tokens: count(usage.cacheWrite),
      total_tokens: count(usage.totalTokens),
    };
    if (typeof message.responseId === "string" && message.responseId) payload.message_id = message.responseId;
    const cost = usage.cost?.total;
    if (typeof cost === "number" && Number.isFinite(cost) && cost >= 0) payload.cost_total = cost;
    send("Usage", payload);
  });

  // agent_end can still be followed by an automatic retry or compaction;
  // agent_settled is the point where Pi will not continue on its own.
  pi.on("agent_settled", (_event, ctx) => {
    if (!interactive || ctx?.isIdle?.() === false) return;
    const failed = lastAssistant?.stopReason === "error";
    const errorMessage = String(lastAssistant?.errorMessage ?? "").slice(0, maxErrorMessage);
    lastAssistant = undefined;
    if (!failed) {
      send("Stop", sessionFields(ctx));
      return;
    }
    const payload: Record<string, unknown> = sessionFields(ctx);
    if (errorMessage) payload.error_message = errorMessage;
    send("StopFailure", payload);
  });
}
