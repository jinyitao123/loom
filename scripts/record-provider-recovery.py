#!/usr/bin/env python3
"""Record a bounded synthetic tool conversation; never persist credentials.

Only explicit API keys are accepted. CI replays the checked-in recordings and
does not run this script or contact a provider.
"""
import argparse
import copy
import datetime
import hashlib
import json
import os
from pathlib import Path
import urllib.error
import urllib.request


SYSTEM = (
    "This is a synthetic protocol test. First call lookup_value for alpha and "
    "beta together in one assistant tool_calls batch. After both tool results, "
    "call sum_values once with values [2,3]. After that result answer exactly "
    "DONE:5. Do not invent results or call any other tool."
)
USER = "Look up the two synthetic values, then add them using the tools."
TOOLS = [
    {"type": "function", "function": {
        "name": "lookup_value", "description": "Read a synthetic alpha or beta value.",
        "parameters": {"type": "object", "properties": {"label": {"type": "string", "enum": ["alpha", "beta"]}},
                       "required": ["label"], "additionalProperties": False}}},
    {"type": "function", "function": {
        "name": "sum_values", "description": "Add the two previously read synthetic values.",
        "parameters": {"type": "object", "properties": {"values": {"type": "array", "items": {"type": "integer"}}},
                       "required": ["values"], "additionalProperties": False}}},
]


def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def digest(value):
    return hashlib.sha256(value).hexdigest()


def key_for(provider, auth_file):
    key = os.environ.get(provider.upper() + "_API_KEY", "")
    if key:
        return key
    if auth_file:
        entry = json.loads(Path(auth_file).read_text()).get(provider, {})
        if entry.get("type") not in [None, "api_key", "apiKey"]:
            raise ValueError("API-key source required; OAuth is not accepted")
        for name in ["key", "apiKey", "api_key"]:
            if isinstance(entry.get(name), str) and entry[name]:
                return entry[name]
    raise ValueError("provider API key unavailable")


def perform(call):
    function = call["function"]
    arguments = json.loads(function["arguments"])
    if function["name"] == "lookup_value" and set(arguments) == {"label"} and arguments["label"] in ["alpha", "beta"]:
        return canonical({"label": arguments["label"], "value": {"alpha": 2, "beta": 3}[arguments["label"]]})
    if function["name"] == "sum_values" and arguments == {"values": [2, 3]}:
        return canonical({"sum": 5})
    raise ValueError("unexpected synthetic tool or arguments")


def assistant(response):
    message = response["choices"][0]["message"]
    return {field: message[field] for field in ["role", "content", "tool_calls", "reasoning_content"] if field in message}


def redact(value):
    if isinstance(value, list):
        return [redact(item) for item in value]
    if isinstance(value, dict):
        output = {}
        for name, content in value.items():
            if name == "reasoning_content" and isinstance(content, str) and content:
                output[name] = "[provider continuation redacted]"
                output["recording_reasoning_sha256"] = digest(content.encode())
                output["recording_reasoning_bytes"] = len(content.encode())
            else:
                output[name] = redact(content)
        return output
    return value


def capture(provider, model, key, probe_thinking):
    endpoint = {"deepseek": "https://api.deepseek.com/v1/chat/completions",
                "openai": "https://api.openai.com/v1/chat/completions"}[provider]
    calls = 0

    def send(body):
        nonlocal calls
        calls += 1
        raw_request = canonical(body).encode()
        request = urllib.request.Request(endpoint, data=raw_request, headers={
            "Authorization": "Bearer " + key, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=45) as response:
                status, raw_response = response.status, response.read()
        except urllib.error.HTTPError as error:
            status, raw_response = error.code, error.read()
        parsed = json.loads(raw_response)
        return {"status": status, "request": copy.deepcopy(body), "response": parsed,
                "raw_request_sha256": digest(raw_request), "raw_response_sha256": digest(raw_response)}

    def body(messages, thinking=False):
        result = {"model": model, "messages": copy.deepcopy(messages), "tools": TOOLS, "max_tokens": 1024 if thinking else 512}
        if provider == "deepseek":
            result.update(thinking={"type": "enabled" if thinking else "disabled"}, reasoning_effort="high" if thinking else "none")
        return result

    initial = [{"role": "system", "content": SYSTEM}, {"role": "user", "content": USER}]
    messages, turns = copy.deepcopy(initial), []
    for _ in range(4):
        turn = send(body(messages))
        if turn["status"] != 200:
            raise ValueError("provider generation failed with HTTP " + str(turn["status"]))
        message = assistant(turn["response"])
        if message.get("reasoning_content"):
            raise ValueError("non-thinking profile unexpectedly returned reasoning")
        turns.append(turn)
        messages.append(message)
        tool_calls = message.get("tool_calls", [])
        if not tool_calls:
            if message.get("content", "").strip() != "DONE:5":
                raise ValueError("provider did not finish the bounded synthetic task")
            break
        for call in tool_calls:
            messages.append({"role": "tool", "tool_call_id": call["id"], "content": perform(call)})
    else:
        raise ValueError("synthetic recording exceeded four model calls")
    if len(turns[0]["response"]["choices"][0]["message"].get("tool_calls", [])) < 2:
        raise ValueError("recording did not include the required multi-tool batch")
    thinking_probe = None
    if probe_thinking:
        if provider != "deepseek":
            raise ValueError("thinking probe is DeepSeek-specific")
        first = send(body(initial, True))
        if first["status"] != 200:
            raise ValueError("thinking first response failed with HTTP " + str(first["status"]))
        message = assistant(first["response"])
        if not message.get("reasoning_content") or not message.get("tool_calls"):
            raise ValueError("thinking response lacked continuation or tool calls")
        followup = copy.deepcopy(initial) + [message]
        for call in message["tool_calls"]:
            followup.append({"role": "tool", "tool_call_id": call["id"], "content": perform(call)})
        positive = send(body(followup, True))
        if positive["status"] != 200:
            raise ValueError("preserved thinking continuation was not accepted")
        omitted = copy.deepcopy(followup)
        omitted[len(initial)].pop("reasoning_content")
        negative = send(body(omitted, True))
        thinking_probe = {"first": first, "preserved": positive, "omitted": negative}
    result = {"version": 1, "provider": provider, "requested_model": model,
              "captured_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "endpoint": endpoint, "synthetic_only": True, "profile": "existing_nonthinking_tools",
              "redacted_fields": ["reasoning_content"], "initial_messages": initial, "tools": TOOLS,
              "turns": turns, "thinking_probe": thinking_probe}
    sanitized = redact(result)
    if key in canonical(sanitized):
        raise ValueError("credential leaked into recording; refusing to write")
    return sanitized, calls


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", choices=["deepseek", "openai"], required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--auth-file")
    parser.add_argument("--thinking-probe", action="store_true")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result, calls = capture(args.provider, args.model, key_for(args.provider, args.auth_file), args.thinking_probe)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"provider": args.provider, "model": args.model, "captured_model_calls": calls,
                      "normal_turns": len(result["turns"]),
                      "thinking_omission_status": result["thinking_probe"]["omitted"]["status"] if result["thinking_probe"] else None,
                      "output": str(args.output)}))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Never emit raw HTTP bodies, headers, credential sources or reasoning.
        print(json.dumps({"recording_failed": True, "error_type": type(error).__name__,
                          "safe_message": str(error) if isinstance(error, ValueError) else "provider recording unavailable"}))
        raise SystemExit(1)
