# Deprecations

> For the complete documentation index, see [llms.txt](/llms.txt). Markdown versions of documentation pages are available by appending `.md` to the page URL.

## Overview

As we launch safer and more capable models, we regularly retire older models. Software relying on OpenAI models may need occasional updates to keep working. Impacted customers will always be notified by email and in our documentation along with [blog posts](https://openai.com/blog) for larger changes.

This page lists all API deprecations, along with recommended replacements.

To monitor model shutdowns programmatically, call the [List models endpoint](https://developers.openai.com/api/reference/resources/models/methods/list) (`GET /v1/models`) and check each model's optional `shutdown_date` field. When present, this field contains the shutdown date as a `YYYY-MM-DD` string, or `null` if no shutdown date has been announced; the field is also available from the [Retrieve a model endpoint](https://developers.openai.com/api/reference/resources/models/methods/retrieve).

## Model deprecation notice periods

We provide advance notice before retiring models so customers have time to plan and migrate. When we announce a model deprecation, we notify customers who are actively using the model by email and document the deprecation on this page.

Unless safety or compliance concerns require a faster timeline, we provide the following minimum notice periods before model retirement:

- **Generally available models:** At least 6 months.
- **Specialized variants of generally available models and image generation models:** At least 3 months. Examples include chat variants such as `gpt-5.1-chat-latest`, Codex variants such as `gpt-5.3-codex`, and deep research variants such as `o3-deep-research`.
- **Preview models:** Preview models, identified by `preview` in the model name, may be retired with much shorter notice, such as 2 weeks. Examples include `computer-use-preview` and `gpt-4o-audio-preview`. We don't recommend using preview models for business-critical production workloads unless you can migrate on short notice.

If safety or compliance concerns require us to retire a model sooner, we will provide as much notice as reasonably possible.

These notice periods give customers time to evaluate recommended replacement models, test application behavior, and complete migrations before a model is no longer available. In some cases, developers may be able to provision dedicated capacity for continued access after a model's shutdown date. To explore this option, [contact our sales team](https://openai.com/contact-sales/).

## Deprecation vs. legacy

We use the term "deprecation" to refer to the process of retiring a model or endpoint. When we announce that a model or endpoint is being deprecated, it immediately becomes deprecated. All deprecated models and endpoints will also have a shut down date. At the time of the shut down, the model or endpoint will no longer be accessible.

We use the terms "sunset" and "shut down" interchangeably to mean a model or endpoint is no longer accessible.

We use the term "legacy" to refer to models and endpoints that no longer receive updates. We tag endpoints and models as legacy to signal to developers where we're moving as a platform and that they should likely migrate to newer models or endpoints. You can expect that a legacy model or endpoint will be deprecated at some point in the future.

## Upcoming deprecations

Upcoming deprecations are listed below, with the most recent announcements at the top.

### 2026-11-10: GPT Image 2

The `gpt-image-2` model and `gpt-image-2-2026-04-21` snapshot are deprecated and will be removed from the API on February 10, 2027.

| Shutdown date     | Model / system                            | Recommended replacement                           |
| ----------------- | ----------------------------------------- | ------------------------------------------------- |
| February 10, 2027 | `gpt-image-2` \| `gpt-image-2-2026-04-21` | `gpt-image-2.6-sunburst` or `gpt-image-2.6-flare` |

### 2026-11-05: GPT-5.5-Cyber

The `gpt-5.5-cyber-preview` model is deprecated and will be removed from the API on February 1, 2027. Migrate to the most capable cyber model available to you before the shutdown date.

| Shutdown date    | Model / system          | Recommended replacement                        |
| ---------------- | ----------------------- | ---------------------------------------------- |
| February 1, 2027 | `gpt-5.5-cyber-preview` | The most capable cyber model available to you. |

### 2026-10-14: GPT-4 series model deprecations

On October 14, 2026, we notified developers using certain GPT-4 series models of their deprecation and removal from the API on January 14, 2027.

| Shutdown date    | Model / system                                    | Recommended replacement |
| ---------------- | ------------------------------------------------- | ----------------------- |
| January 14, 2027 | GPT-4o (`gpt-4o-2024-08-06`, `gpt-4o-2024-11-20`) | `gpt-6-luna`            |
| January 14, 2027 | GPT-4o mini (`gpt-4o-mini-2024-07-18`)            | `gpt-6-luna`            |
| January 14, 2027 | GPT-4.1 (`gpt-4.1-2025-04-14`)                    | `gpt-6-luna`            |
| January 14, 2027 | GPT-4.1 mini (`gpt-4.1-mini-2025-04-14`)          | `gpt-6-luna`            |

Fine-tuned versions of these models follow the same shutdown schedule as their underlying base models.

### 2026-10-01: GPT-5.3-Codex, GPT-5.1, GPT-5.4-Nano

The following models are deprecated and will be removed from the API on April 1, 2027, with six months' notice. Migrate to the recommended replacements before the shutdown date.

| Shutdown date | Model / system                              | Recommended replacement |
| ------------- | ------------------------------------------- | ----------------------- |
| Apr 1, 2027   | `gpt-5.3-codex`                             | `gpt-6-sol`             |
| Apr 1, 2027   | `gpt-5.4-nano` \| `gpt-5.4-nano-2026-03-17` | `gpt-6-luna`            |
