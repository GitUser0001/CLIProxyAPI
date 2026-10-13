# OpenAI Decisions API

This fork adds authenticated `POST /v1/decisions` with the native OpenAI request
and response schema. Text, image inputs, predicate, choice, score, and refusal
results pass through without chat translation. The default model is `gpt-6-luna`.

Configure a dedicated upstream key under the v8 configuration:

```yaml
upstream:
  openai-decisions:
    enabled: true
    api-key-file: /run/credentials/cliproxyapi.service/openai-decisions-key
    base-url: https://api.openai.com/v1
    models: [gpt-6-luna]
```

The key file must be an absolute path to a regular file with no group/other
permissions (0600 or stricter). It must be readable by the service. Use systemd
`LoadCredential=openai-decisions-key:/path/to/private/key` to keep the original
root-private; systemd loads changes on service restart. Ordinary key files are
read on each request and support replacement without restarting the proxy.
Never place an API key in this configuration or the shared chat-provider pool.

Existing proxy client keys authenticate requests. The dedicated upstream key is
available only to this endpoint; it cannot serve chat completions, Responses,
images, or other routes, and is not added to the global model registry. Disabled
or missing configuration returns 503, anonymous requests return 401. Configure
via native v8 management mutation at
`/v8/management/config/upstream/openai-decisions`.

One upstream attempt is made per accepted request, with no automatic retry or
OAuth fallback. The upstream status, JSON result, request ID, rate-limit headers,
and Retry-After are preserved. Redirects are never followed. Caller cancellation
cancels upstream work. Requests and responses are capped at 64 MiB. HTTPS is
required for remote upstreams; loopback HTTP is allowed for isolated tests.

Payload rules can target `protocol: decisions`; they run as the final semantic
barrier. No tools, reasoning fields, or chat instructions are injected. Reported
token usage and upstream failures flow through the existing usage queue under
provider `openai-decisions` with the original proxy client identity. The upstream
key is never included in usage metadata. Cost estimation for this new provider
requires a separate verified pricing entry; do not assume chat-model rates.

On 2026-10-13, the installed Codex subscription OAuth credential returned 404 at
`https://chatgpt.com/backend-api/codex/decisions` and 401 (`A valid API key is
required.`) at `https://api.openai.com/v1/decisions`. This feature therefore uses
an OpenAI platform API key; it does not emulate Decisions with chat generation.

Official request schema: https://developers.openai.com/api/docs/guides/decisions
