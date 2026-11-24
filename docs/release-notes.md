Fix upstream overload appearing as an empty successful response during consecutive requests.

The server reproduction sent six consecutive requests and two more after a 45-second pause. Request six had HTTP 200 and no text: BPS returned `server_is_overloaded` after its lifecycle events, with a seven-second retry hint. Requests after the pause succeeded.

- Hold startup lifecycle events until actual text/reasoning output, a validated client tool event or a valid terminal response. An earlier overload can now return HTTP 503; rate limits, authentication failures and model rejection retain their status.
- Preserve upstream retry hints, including millisecond values, with bounded parsing and no arbitrary upstream message disclosure.
- Preserve partial output and the original failure event when a stream fails after content has started. Do not synthesize a completed response or replay generation.
- Wait for the copied bridge's normal WebSocket cleanup and task slot release instead of cancelling a terminal connection early.
- Handle CPA's bounded pre-opened stream queue without deadlocking a slow failed startup; cancellation and plugin shutdown continue to drain owned work.

Compared directly with the current `bps-local-public` working tree: all 87 core Go files are present, 85 match after module-path normalization, and the only two differences are the documented request-credential adapter and Windows ACL test correction. Core protocol, task scheduling, tools and accounting remain unchanged.

Store source:

```text
https://raw.githubusercontent.com/zyxzjyzjj/cpa-gptbps-plugin/main/registry.json
```

Upgrade the plugin and restart CPA. Authenticated upstream model discovery from v0.1.2 remains in place, with the same default 300-second account-reload cache behavior.

Focused race checks passed, including startup failure, retry metadata, function/custom tool-only output, partial output followed by failure, cancellation, delayed close acknowledgement and bounded host queue backpressure. Release CI and live upgraded-server results are recorded in [WORKLOG.md](https://github.com/zyxzjyzjj/cpa-gptbps-plugin/blob/main/docs/WORKLOG.md).

BPS may still reject or overload requests. If content has already reached the client, HTTP status cannot be changed; clients must handle a terminal stream error. Appearing in the model catalog does not prove generation entitlement or available quota.
