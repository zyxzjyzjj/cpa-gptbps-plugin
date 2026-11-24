Initial Codex BPS native provider plugin for CLIProxyAPI.

- Reuses the GPT-BPS protocol core, account task pool, client tools and PKCE authorization.
- Adds CPA credential import, login polling, guarded refresh and a bundled management page.
- Preserves real upstream usage for native CPA request statistics and CPA-Manager-Plus.
- Ships native libraries, the desktop login companion, SHA-256 checksums and a third-party store registry.

All five native build/test jobs passed. On Windows amd64, the real CPA host completed anonymous third-party-store installation, authenticated credential import and real BPS generation. Text, function/custom tool round trips, cache reads, HTTPS images, structured JSON and cancellation were tested. CPA-Manager-Plus's original parser and SQLite storage accepted 16 real usage records without duplicate insertion.

Store source:

```text
https://raw.githubusercontent.com/zyxzjyzjj/cpa-gptbps-plugin/main/registry.json
```

Read [the validation record](https://github.com/zyxzjyzjj/cpa-gptbps-plugin/blob/main/docs/WORKLOG.md) and the attached `validation-results.json` for actual results and limits. ZIP documentation is a build-time snapshot; later store acceptance is recorded on main and in the attached report.

Hosted tools, inline images, previous_response_id, forced tool_choice and compact/count_tokens remain unsupported by this bridge. BPS requires its own OAuth client; native Codex grants are not interchangeable. Nonstream C ABI calls have no separate downstream cancellation signal. RT rotation was verified with fixtures, not forced on the live account. Linux/macOS live-host loading was not tested.
