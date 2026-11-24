Fix the provider exposing only the single model configured by v0.1.0.

- Discover each credential's models from the authenticated BPS access endpoint (`include_models=true`), reading `model_catalog.models`.
- Preserve upstream IDs, labels, descriptions and bridge-supported reasoning levels; filter restricted models.
- Remove fixed/default model registration. Legacy `models` lists no longer limit or supplement the catalog.
- Isolate the catalog cache by credential, account and proxy; deduplicate concurrent requests, revalidate expired entries and cancel discovery during shutdown.
- Authentication/permission errors clear the cached catalog. Temporary failures may use the last actual upstream catalog for a bounded period and are disclosed as stale in status; no hardcoded fallback is added.

Store source:

```text
https://raw.githubusercontent.com/zyxzjyzjj/cpa-gptbps-plugin/main/registry.json
```

Upgrade the plugin and restart CPA to load the live account catalog. Default `model_cache_seconds` is 300. An expired cache is revalidated when CPA next reloads that account's models; this is not an independent background refresh timer.

The BPS account used for verification returned 9 models with HTTP 200. Focused plugin tests and race checks passed. Release-build and server verification results are recorded in [WORKLOG.md](https://github.com/zyxzjyzjj/cpa-gptbps-plugin/blob/main/docs/WORKLOG.md).

The inference bridge, task pool, tools and native CPA usage reporting are unchanged. Existing BPS restrictions still apply; appearing in the model catalog does not prove generation entitlement, remaining quota or every tool capability.
