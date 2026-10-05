// Switchboard lifecycle reporting for Pi (https://pi.dev).
//
// Install: copy or symlink into ~/.pi/agent/extensions/. It sits beside herdr's
// own managed extension and does not touch it: herdr keeps reporting
// working/blocked/idle to herdr, and this reports to the Switchboard daemon
// through `switchboard-ctl pi-hook <event>` (stdin = JSON), the way Claude Code
// and Codex hooks do.
//
// Events, in Claude Code's hook vocabulary so the daemon has one table:
//   agent_start                         → UserPromptSubmit (new activity)
//   agent_settled after a failed run    → StopFailure {error_message}
//
// Only the error text of a failed run is sent, and switchboard-ctl reduces it
// to a usage-limit verdict before anything reaches the daemon. Full lifecycle
// reporting is queued in docs/usage-limit-status-plan.md.
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

export default function (pi) {
  let interactive = false;
  let lastAssistant: { stopReason?: string; errorMessage?: string } | undefined;

  pi.on("session_start", (_event, ctx) => {
    // RPC/JSON/print runs have no pane to colour; mirror herdr's gate.
    interactive = ctx?.mode === "tui";
  });

  pi.on("agent_start", (_event, ctx) => {
    if (!interactive) return;
    lastAssistant = undefined;
    send("UserPromptSubmit", sessionFields(ctx));
  });

  pi.on("message_end", (event) => {
    const message = event?.message;
    if (message?.role !== "assistant") return;
    lastAssistant = { stopReason: message.stopReason, errorMessage: message.errorMessage };
  });

  // agent_end can still be followed by an automatic retry or compaction;
  // agent_settled is the point where Pi will not continue on its own.
  pi.on("agent_settled", (_event, ctx) => {
    if (!interactive || lastAssistant?.stopReason !== "error") return;
    const errorMessage = String(lastAssistant.errorMessage ?? "").slice(0, maxErrorMessage);
    lastAssistant = undefined;
    if (!errorMessage) return;
    send("StopFailure", { ...sessionFields(ctx), error_message: errorMessage });
  });
}
