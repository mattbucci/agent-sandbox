#!/usr/bin/env python3
"""A scripted stand-in for `omp --mode rpc`, for test_omp_agent.py.

Speaks the subset of the RPC protocol the adapter uses. The prompt text selects
the behaviour:

  echo:<text>   stream <text> (preceded by blank lines, as real models do)
  count         answer with how many prompts this session dir has seen
  bash:<cmd>    request approval for a shell command, then report the decision
  dialog        raise a non-approval dialog and report how it was answered
  fail          end the turn with a provider error
  hang          never finish until aborted
  die           exit mid-turn
"""
import json
import os
import sys


def out(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def read():
    line = sys.stdin.readline()
    if not line:
        sys.exit(0)
    return json.loads(line)


def assistant(text, usage_in=10, usage_out=3):
    return {"role": "assistant", "content": [{"type": "thinking", "thinking": "hm"},
                                             {"type": "text", "text": text}],
            "usage": {"input": usage_in, "output": usage_out}, "stopReason": "stop"}


def say(text):
    out({"type": "message_update", "assistantMessageEvent": {"type": "thinking_delta", "delta": "hm"}})
    for piece in ("\n\n", text[: len(text) // 2], text[len(text) // 2:]):
        out({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": piece}})
    out({"type": "message_end", "message": assistant("\n\n" + text)})


def main():
    args = sys.argv[1:]
    session_dir = args[args.index("--session-dir") + 1]
    os.makedirs(session_dir, exist_ok=True)
    counter = os.path.join(session_dir, "count")
    with open(os.path.join(session_dir, "argv.json"), "w") as f:
        json.dump(args, f)

    out({"type": "ready", "protocolVersion": 1})
    out({"type": "extension_ui_request", "id": "w1", "method": "setWidget", "widgetKey": "x"})

    while True:
        cmd = read()
        if cmd.get("type") != "prompt":
            continue
        pid, msg = cmd["id"], cmd["message"]
        out({"id": pid, "type": "response", "command": "prompt", "success": True})
        out({"type": "message_end", "message": {"role": "user", "content": [{"type": "text", "text": msg}]}})
        n = int(open(counter).read()) + 1 if os.path.exists(counter) else 1
        with open(counter, "w") as f:
            f.write(str(n))

        if msg.startswith("echo:"):
            say(msg[5:])
        elif msg == "count":
            say(f"prompt {n}")
        elif msg.startswith("bash:"):
            command = msg[5:]
            out({"type": "tool_execution_start", "toolCallId": "c1", "toolName": "bash",
                 "args": {"command": command}, "intent": "run it"})
            out({"type": "extension_ui_request", "id": "a1", "method": "select",
                 "title": f"Allow tool: bash\nCommand: {command}",
                 "options": ["Approve", "Approve bash for this session", "Deny"]})
            answer = read()
            approved = answer.get("value") == "Approve"
            out({"type": "tool_execution_end", "toolCallId": "c1", "toolName": "bash",
                 "isError": not approved})
            say("ran" if approved else "denied")
        elif msg == "dialog":
            out({"type": "extension_ui_request", "id": "d1", "method": "confirm",
                 "title": "Confirm", "message": "Continue?"})
            answer = read()
            say("cancelled" if answer.get("cancelled") else "answered")
        elif msg == "fail":
            out({"type": "prompt_result", "id": pid, "agentInvoked": True, "status": "error",
                 "error": {"message": "no healthy backend", "retryable": False}})
            continue
        elif msg == "hang":
            while True:
                nxt = read()
                if nxt.get("type") == "abort":
                    out({"type": "prompt_result", "id": pid, "agentInvoked": True, "status": "aborted"})
                    break
            continue
        elif msg == "die":
            sys.exit(3)
        out({"type": "prompt_result", "id": pid, "agentInvoked": True, "status": "completed",
             "sessionSettled": True})


if __name__ == "__main__":
    main()
