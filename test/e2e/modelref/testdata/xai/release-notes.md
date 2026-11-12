#### Release Notes

# Release Notes

## November

### grok-imagine-image-quality retired

Requests to `grok-imagine-image-quality` and `grok-imagine-image-pro` are now served by `grok-imagine-image-2.0-low`: `grok-imagine-image-2.0` with `quality` set to `low`, at a lower per-image price. No request changes are needed. `usage.cost_in_usd_ticks` on each response reflects the lower price, and gRPC responses (xAI SDK) report `grok-imagine-image-2.0-low` as the `model`. See the [migration guide](/developers/migration/imagine-image-quality-nov-2).

## October

### Custom moderation controls

You can now set content guidelines on text API requests. Add a `guidelines` array to a Chat Completions or Responses API request to set how Grok handles content in the `nsfw`, `violence`, and `profanity` categories. Grok follows the guidelines as it writes its response. See [Custom Moderation Controls](/developers/advanced-api-usage/custom-moderation-controls).

### Grok Voice 3.0 is available

`grok-voice-3.0` is now available with Speech to Speech, at the same price as Grok Voice 2.0. `grok-voice-latest` now points to `grok-voice-3.0`, so sessions that use the alias are upgraded automatically. The API is unchanged. To get started, see the [Speech to Speech docs](/developers/model-capabilities/audio/speech-to-speech).

### Grok Voice 2.0 deprecation

`grok-voice-2.0` is deprecated. To upgrade, set `model` to `grok-voice-3.0` or `grok-voice-latest`, at the same price. See [Upgrading from Grok Voice 2.0](/developers/model-capabilities/audio/speech-to-speech#upgrading-from-grok-voice-20).

### grok-voice-transcribe-1.0 end of life

`grok-voice-transcribe-1.0` is deprecated and reaches end of life on October 2, 2026. All requests to that slug are routed to `grok-voice-transcribe-2.0` at the same price, with higher accuracy. See the [Speech to Text docs](/developers/model-capabilities/audio/speech-to-text).

## September

### safety\_identifier request field

You can now send `safety_identifier`, an opaque end-user identifier assigned by your application, on Chat Completions, the Responses API, deferred chat completions, the Batch API, and the gRPC `GetCompletionsRequest`. It lets SpaceXSI attribute a policy violation to one of your end users rather than to your API key. The legacy `user` field is still accepted. See the [Security FAQ](/developers/faq/security#how-do-i-identify-which-of-my-end-users-caused-a-policy-violation).

### Grok 4.7

Grok 4.7, SpaceXSI's frontier model for coding, agentic tasks, and knowledge work, is now available on the xAI API as `grok-4.7`. It has a 500k context window, text and image inputs with text-only output, and no text output limit. Pricing is $2 / $0.50 / $6 per 1M tokens (input / cached input / output) below 200k prompt tokens, and $4 / $1 / $12 above. Reasoning effort supports low, medium, high (default), and xhigh. On the Responses API, `grok-4.7` always returns `reasoning.encrypted_content`, even when `include` does not list it. It is also served on the [US regional endpoint](/developers/advanced-api-usage/regions). Grok 4.7 Fast is the same model and costs 2x the standard token rates, or 1.5x for long-context requests. It's available only in Cursor and Grok Build, not on the public xAI API, and is billed through your plan there. See the [Grok 4.7 overview](/developers/grok-4-7) and the [announcement](https://x.ai/news/grok-4-7).

### Grok Voice Transcribe 2.0

