# A fresh developer machine for the live suite's machine scenarios (`make machines`):
# Linux, a non-root user with an empty home, git, Node, and the agents from npm. terma
# and the test binary are built for Linux on the host and mounted in, so the image is
# rebuilt only when an agent version changes.
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates procps \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_VERSION=2.1.285
ARG CODEX_VERSION=0.158.0
# The agents' own install scripts fetch their native builds.
RUN npm install -g --ignore-scripts=false \
      "@anthropic-ai/claude-code@${CLAUDE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && claude --version && codex --version

RUN useradd --create-home --shell /bin/bash dev \
 && mkdir -p /etc/codex /etc/claude-code \
 && chown dev /etc/codex /etc/claude-code
# The managed-configuration scenario deploys terma's hooks where the agents read an
# organization's configuration from; the machine user may write there, nobody else runs.
USER dev
ENV TERMA_MACHINE=1
WORKDIR /home/dev
