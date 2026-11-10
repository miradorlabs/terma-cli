# Changelog

> For the complete documentation index, see [llms.txt](/llms.txt). Markdown versions of documentation pages are available by appending `.md` to the page URL.

> The latest features and updates to the OpenAI API.

Upcoming deprecations are listed on the [deprecations page](/api/docs/deprecations).

## November, 2026

### Nov 5

Feature · Model: gpt-6-cyber

Use [GPT-6-Cyber](https://developers.openai.com/api/docs/models/gpt-6-cyber) to investigate vulnerabilities, reproduce them in an authorized test environment, and verify fixes. See [Daybreak setup](https://developers.openai.com/api/docs/guides/daybreak#set-up-gpt-6-cyber) before sending your first request.

### Nov 4

Feature · Model: gpt-image-2.6-sunburst · Model: gpt-image-2.6-flare · API: v1/images/generations · API: v1/images/edits

Released [GPT-Image-2.6 Sunburst](https://developers.openai.com/api/docs/models/gpt-image-2.6-sunburst) and [GPT-Image-2.6 Flare](https://developers.openai.com/api/docs/models/gpt-image-2.6-flare) for image generation and editing, with adjustable complexity, SVG generation, and animated GIF generation. Added markup-guided editing for supported GPT Image 2.5 and 2.6 models.

See the [image generation guide](https://developers.openai.com/api/docs/guides/image-generation#gpt-image-26).

### Nov 2

Feature · Model: gpt-live-1 · API: v1/live/sessions

Outbound SIP calling is now available to all organizations through the Live API. Connect GPT-Live to a phone number through your SIP provider. See the [SIP guide](https://developers.openai.com/api/docs/guides/voice-sip?api=live#place-an-outbound-call).

## October, 2026

### Oct 30

Update · API: v1/agents · API: v1/agents/sessions

You can now [update the tools in an existing session](https://developers.openai.com/api/docs/guides/agents-api/configuration#update-settings-for-an-existing-session) in the Agents API public beta. Updates replace the full tool list. Function tools and MCP servers change on the next model call; built-in tools change by the next turn.

Context compaction now runs in the background by default. Set `compaction.async` to `false` for blocking compaction. Background compaction may increase token usage.

