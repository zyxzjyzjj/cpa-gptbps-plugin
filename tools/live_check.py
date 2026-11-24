"""Real upstream acceptance against an isolated CPA host.

Never run queue draining against a production CPAMP consumer. All raw records
stay in the caller-selected private output directory, outside the source tree.
The terminal prints only synthetic answers, numeric usage and pass/fail facts.
"""
import argparse
import json
from pathlib import Path
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", default="http://127.0.0.1:18317")
    parser.add_argument("--keys", type=Path, required=True,
                        help="Private JSON with client and management keys")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--model", default="gpt-5.6-sol")
    parser.add_argument("--drain-private-queue", action="store_true",
                        help="Consume the isolated CPA test queue after requests")
    parser.add_argument("--resume", action="store_true",
                        help="Reuse successful case evidence from this private output directory")
    args = parser.parse_args()
    out = args.out.resolve()
    repo = Path(__file__).resolve().parents[1]
    if out == repo or repo in out.parents:
        parser.error("--out must be a private directory outside the repository")
    out.mkdir(parents=True, exist_ok=True)
    keys = json.loads(args.keys.read_text(encoding="utf-8-sig"))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    results = []
    previous = {}
    if args.resume and (out / "summary.json").exists():
        previous = {item["case"]: item for item in json.loads((out / "summary.json").read_text(encoding="utf-8"))}

    def save_results():
        (out / "summary.json").write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    def call(name, body, path="/v1/responses", expected=200):
        if previous.get(name, {}).get("pass") and (out / (name + ".json")).exists():
            results.append(previous[name])
            print("Reused successful evidence: " + name, flush=True)
            return json.loads((out / (name + ".json")).read_text(encoding="utf-8"))
        body = dict(body, model=args.model)
        started = time.monotonic()
        request = urllib.request.Request(
            args.base.rstrip("/") + path,
            data=json.dumps(body).encode(),
            headers={"Authorization": "Bearer " + keys["client"],
                     "Content-Type": "application/json"},
        )
        status = 0
        data = {}
        try:
            with opener.open(request, timeout=240) as response:
                status = response.status
                if body.get("stream"):
                    events = []
                    for line in response:
                        if line.startswith(b"data:"):
                            chunk = line[5:].strip()
                            if chunk and chunk != b"[DONE]":
                                events.append(json.loads(chunk))
                    (out / (name + "-events.json")).write_text(
                        json.dumps(events, ensure_ascii=False), encoding="utf-8")
                    terminal = [event.get("response") for event in events
                                if event.get("type") in ("response.completed", "response.incomplete")]
                    data = terminal[-1] if terminal else {"error": "no terminal response"}
                else:
                    data = json.load(response)
        except urllib.error.HTTPError as error:
            status = error.code
            data = json.loads(error.read())
        (out / (name + ".json")).write_text(json.dumps(data, ensure_ascii=False), encoding="utf-8")
        result = {"case": name, "http_status": status, "expected_status": expected,
                  "seconds": round(time.monotonic() - started, 3),
                  "usage": data.get("usage"), "status": data.get("status")}
        result["pass"] = status == expected and (expected != 200 or "error" not in data or data["error"] is None)
        results.append(result)
        save_results()
        print(json.dumps(result, ensure_ascii=False), flush=True)
        return data

    simple = {"input": "Reply exactly OK.", "reasoning": {"effort": "low"}, "stream": False}
    call("text_nonstream", simple)
    call("text_cached_stream", dict(simple, stream=True))

    tool = {"type": "function", "name": "sum_numbers",
            "description": "Return a plus b. This tool is executed by the client.",
            "parameters": {"type": "object", "properties": {
                "a": {"type": "integer"}, "b": {"type": "integer"}},
                "required": ["a", "b"], "additionalProperties": False}, "strict": True}
    history = [{"role": "user", "content": "Call sum_numbers with a=19 and b=23. Do not answer before calling it."}]
    first = call("function_call", {"input": history, "tools": [tool], "stream": True,
                                  "tool_choice": "auto",
                                  "reasoning": {"effort": "low"}})
    calls = [item for item in first.get("output", []) if item.get("type") == "function_call"]
    valid = len(calls) == 1 and calls[0].get("name") == "sum_numbers"
    if valid:
        arguments = json.loads(calls[0]["arguments"])
        valid = arguments == {"a": 19, "b": 23}
        history += first["output"] + [{"type": "function_call_output", "call_id": calls[0]["call_id"],
                                       "output": str(arguments["a"] + arguments["b"])}]
        follow = call("function_result", {"input": history, "tools": [tool],
                                         "stream": False, "reasoning": {"effort": "low"}})
        valid = valid and "42" in json.dumps(follow.get("output", []))
    results.append({"case": "function_roundtrip", "pass": valid})
    print(json.dumps(results[-1]), flush=True)

    custom = {"type": "custom", "name": "echo_literal",
              "description": "Pass exact raw input BPS_TOOL_OK; client echoes that literal.",
              "format": {"type": "text"}}
    custom_history = [{"role": "user", "content": "Call echo_literal with raw input BPS_TOOL_OK, then wait for the tool result."}]
    first = call("custom_call", {"input": custom_history, "tools": [custom], "stream": False,
                                "tool_choice": "auto",
                                "reasoning": {"effort": "low"}})
    calls = [item for item in first.get("output", []) if item.get("type") == "custom_tool_call"]
    valid = len(calls) == 1 and calls[0].get("name") == "echo_literal" and calls[0].get("input") == "BPS_TOOL_OK"
    if valid:
        custom_history += first["output"] + [{"type": "custom_tool_call_output",
                                             "call_id": calls[0]["call_id"], "output": "BPS_TOOL_OK"}]
        follow = call("custom_result", {"input": custom_history, "tools": [custom],
                                       "stream": True, "reasoning": {"effort": "low"}})
        valid = "BPS_TOOL_OK" in json.dumps(follow.get("output", []))
    results.append({"case": "custom_roundtrip", "pass": valid})
    print(json.dumps(results[-1]), flush=True)

    structured = call("structured_json", {"input": "Return an object with ok equal to true.",
                      "stream": False, "reasoning": {"effort": "low"},
                      "text": {"format": {"type": "json_schema", "name": "check",
                               "strict": True, "schema": {"type": "object",
                                  "properties": {"ok": {"type": "boolean", "const": True}},
                                  "required": ["ok"], "additionalProperties": False}}}})
    texts = [part["text"] for item in structured.get("output", [])
             for part in item.get("content", []) if part.get("type") == "output_text"]
    results.append({"case": "structured_schema", "pass": bool(texts) and json.loads("".join(texts)) == {"ok": True}})

    call("chat_nonstream", {"messages": [{"role": "user", "content": "Reply exactly CHAT_OK."}],
                           "stream": False, "reasoning_effort": "low"}, path="/v1/chat/completions")
    call("reject_previous_response", dict(simple, previous_response_id="resp_synthetic_unsupported"), expected=400)
    call("reject_inline_image", {"stream": False, "input": [{"role": "user", "content": [
        {"type": "input_text", "text": "What is shown?"},
        {"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="}]}]}, expected=400)
    call("reject_forced_tool_choice", {"input": "Call sum_numbers.", "tools": [tool], "stream": False,
         "tool_choice": {"type": "function", "name": "sum_numbers"}}, expected=400)
    call("reject_compact", simple, path="/v1/responses/compact", expected=501)

    if args.drain_private_queue:
        request = urllib.request.Request(
            args.base.rstrip("/") + "/v0/management/usage-queue?count=1000",
            headers={"Authorization": "Bearer " + keys["management"]})
        with opener.open(request, timeout=20) as response:
            queue = json.load(response)
        normalized = [json.loads(item) if isinstance(item, str) else item for item in queue]
        if args.resume and (out / "usage-queue.json").exists():
            earlier = json.loads((out / "usage-queue.json").read_text(encoding="utf-8"))
            unique = {item["request_id"]: item for item in earlier + normalized}
            normalized = list(unique.values())
        (out / "usage-queue.json").write_text(json.dumps(normalized, ensure_ascii=False), encoding="utf-8")
        print("Private native queue records:", len(normalized), flush=True)
        for record in normalized:
            print(json.dumps({key: record.get(key) for key in (
                "provider", "executor_type", "model", "stream", "failed", "latency_ms", "ttft_ms", "tokens")}), flush=True)
    (out / "summary.json").write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print("All checks passed:", all(item["pass"] for item in results), flush=True)


if __name__ == "__main__":
    main()
