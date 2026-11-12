---
title: What's new in Claude Fable 5.2
url: https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2
description: Breaking changes, feature support, pricing, and availability for Claude Fable 5.2 and Claude Mythos 5.2, compared with Claude Fable 5.1 and Claude Mythos 5.1.
---

Claude Fable 5.2 is built for demanding reasoning and long-horizon agentic work. For most workloads, start with the latest Claude Opus model (see [Choosing the right model](https://platform.claude.com/docs/en/about-claude/models/choosing-a-model)), and move to Claude Fable 5.2 when your evals on a Claude Opus model at higher effort still don’t meet your requirements.

Four changes from Claude Fable 5.1 can break your code:

* [The prefix check applies to every account](https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2#editing-earlier-turns-invalidates-thinking-blocks).
* [Only Claude Fable 5.2 and Claude Mythos 5.2 read Claude Fable 5.2 thinking blocks](https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2#thinking-blocks-are-tied-to-the-model-that-produced-them).
* The earlier [`computer_20251124` computer use tool returns an error](https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2#computer-20251124-is-not-supported) on every platform except Amazon Bedrock.
* With the advisor tool, a Claude Fable 5.2 executor [needs a Claude Fable 5.2 or Claude Mythos 5.2 advisor](https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2#advisor-tool-pairs).

## Models

| Model             | Claude API ID     | Description                                           | Availability                                                                                                                                                |
| ----------------- | ----------------- | ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claude Fable 5.2  | claude-fable-5-2  | For demanding reasoning and long-horizon agentic work | All customers on the Claude API (see each platform’s page for partner platforms)                                                                            |
| Claude Mythos 5.2 | claude-mythos-5-2 | Same capabilities as Claude Fable 5.2                 | Organizations verified through Anthropic’s verification programs, such as the [Cyber Verification Program](https://support.claude.com/en/articles/14604842) |

Claude Fable 5.2 and Claude Mythos 5.2 share these specs:

* **Context window and output:** a [1M token context window](https://platform.claude.com/docs/en/build-with-claude/context-windows) (default and maximum) and 128k max output tokens.
* **Thinking:** [adaptive thinking](https://platform.claude.com/docs/en/build-with-claude/thinking) is always on. Use the [effort parameter](https://platform.claude.com/docs/en/build-with-claude/effort) to control thinking depth.
* **Tokenizer:** the tokenizer introduced with Claude Opus 4.7. See [Token counting](https://platform.claude.com/docs/en/build-with-claude/token-counting#token-counts-on-claude-fable-5).

For all current models, see [Models overview](https://platform.claude.com/docs/en/models/overview).

## Breaking changes

### Editing earlier turns invalidates thinking blocks on every account

The API’s prefix check (see [Keeping the prefix unchanged](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking#prefix-check)) tests whether anything before a Claude Fable 5.2 or Claude Mythos 5.2 thinking block has changed since the model produced it: the `system` prompt, the `tools`, or an earlier message. On Claude Fable 5.2 and Claude Mythos 5.2, a request that sends back an invalid thinking block and leaves `prefix_mismatch_behavior` unset gets a 400 error, whatever the account’s age (see [When `prefix_mismatch_behavior` is unset](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking#enforcement)). On Claude Fable 5.1, such a request gets the error on accounts created on or after August 31, 2026. On the Claude API, Claude Mythos 5.1 already returns it on every account. So code on Claude Fable 5.1 that edits its history can run without errors on an older account before the switch and fail after it.

When the check fails, the request returns a 400 error whose message says `The block is bound to a different conversation`. To drop the affected blocks and continue instead, send the `thinking-binding-controls-2026-08-01` beta header and set `thinking.block_binding.prefix_mismatch_behavior` to `"drop_block"`. The response then lists each dropped block in a top-level `input_transformations` array. Treat `"drop_block"` as a stopgap while you fix the edit, because [dropping blocks can raise output tokens](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking#drop-block-cost).

Nothing changes for you if Claude Code, claude.ai, [Claude Managed Agents](https://platform.claude.com/docs/en/managed-agents/overview), or the [Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk/overview) builds your requests.

If your own code builds your requests, nothing changes for you either, as long as all of these are true:

* Your code keeps `system` and `tools` the same for the whole conversation.
* Your code only adds to `messages`. It never changes, reorders, or removes an earlier message.
* Your code [sends each assistant turn back exactly as the API returned it](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking#append-assistant-turns-exactly-as-returned): every block, in the same order.
* Your code does none of the things listed in [What to check in your integration](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking#what-to-check).

Otherwise, test your code before you switch:

* **Code on Claude Fable 5.1:** Follow the [prefix check section](https://platform.claude.com/docs/en/models/fable-5-2/migration-guide#fable-5-2-preserved-thinking) of the migration guide.
* **Code on Claude Mythos 5.1:** Follow [Migrating to Claude Mythos 5.2 from Claude Mythos 5.1](https://platform.claude.com/docs/en/models/fable-5-2/migration-guide#migrating-from-claude-mythos-5-1-to-claude-mythos-5-2).

[Preserved thinking](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking) explains what counts as an edit. It also shows how to make each change without an edit.

### Only Claude Fable 5.2 and Claude Mythos 5.2 read Claude Fable 5.2 thinking blocks

Every thinking block records which model produced it, and a model reads only its own blocks and some other models’ blocks. On the Claude API and Claude Models on Google Cloud (Google managed), Claude Fable 5.2 and Claude Mythos 5.2 read each other’s thinking blocks and those from Claude Opus 5.5, Claude Sonnet 5.5, Claude Haiku 5.5, Claude Fable 5.1, Claude Mythos 5.1, Claude Opus 5, Claude Fable 5, and Claude Mythos 5, and from earlier Opus, Sonnet, and Haiku models. No other model reads their blocks.

* **Moving onto Claude Fable 5.2 or Claude Mythos 5.2** from one of those models keeps the conversation’s reasoning on the Claude API and Claude Models on Google Cloud (Google managed). So does moving between Claude Fable 5.2 and Claude Mythos 5.2.
* **Moving from Claude Fable 5.2 or Claude Mythos 5.2 to any other model** loses their reasoning for the turns that run there. That includes Claude Opus 5.5, Claude Fable 5.1, Claude Mythos 5.1, and Claude Fable 5.2’s fallback models (Claude Opus 4.8 and Claude Opus 5).

When a request carries a block the current model can’t read, the API drops the block before the model receives it. The request succeeds, and dropped blocks aren’t billed. With the `thinking-binding-controls-2026-08-01` beta header, the response lists the drop in `input_transformations`. See [Switching models mid-conversation](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking#switching-models).

### Use the computer use toolset instead of `computer_20251124`

Claude Fable 5.1 accepts [computer use](https://platform.claude.com/docs/en/agents-and-tools/tool-use/computer-use-tool) both as the `computer_toolset_20260801` toolset and, with the `computer-use-2025-11-24` beta header, as the earlier `computer_20251124` tool. Claude Fable 5.2 and Claude Mythos 5.2 support computer use through the toolset. A request to either model that declares a `computer_20251124` tool returns a 400 `invalid_request_error` on every platform except Amazon Bedrock, where the earlier tool works as it does on Claude Fable 5.1.

To move an existing integration, follow [Migrate from `computer_20251124`](https://platform.claude.com/docs/en/agents-and-tools/tool-use/computer-use-tool#migrate-from-computer-20251124).

### A Claude Fable 5.2 executor needs a Claude Fable 5.2 or Claude Mythos 5.2 advisor

With the [advisor tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool), which is in beta, the advisor must be at least as capable as the executor. A Claude Fable 5.2 or Claude Mythos 5.2 executor accepts only Claude Fable 5.2 or Claude Mythos 5.2 as its advisor. Any other advisor, including Claude Fable 5.1 and Claude Mythos 5.1, returns a 400 `invalid_request_error`.

If you use the advisor tool and move the executor to one of these models, set the `model` in the advisor tool definition to `claude-fable-5-2` or `claude-mythos-5-2` in the same change. Changing the advisor’s `model` is an edit to `tools`, so make the change in new conversations. A request that sends earlier thinking blocks back after the change fails the [prefix check](https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2#editing-earlier-turns-invalidates-thinking-blocks) and returns a 400 error, unless you set `thinking.block_binding.prefix_mismatch_behavior` to `"drop_block"` (beta, `thinking-binding-controls-2026-08-01` header). To keep your current executor and change only the advisor, set the advisor’s `model` to `claude-fable-5-2`: every executor model accepts it. A Claude Fable 5.2 advisor returns its advice encrypted, so if your code reads the advice text, see [Result variants](https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool#result-variants). See also [Model compatibility](https://platform.claude.com/docs/en/agents-and-tools/tool-use/advisor-tool#model-compatibility).

## Thinking and tool choice
