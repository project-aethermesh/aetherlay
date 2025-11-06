FROM golang:1.25-trixie

# Run as root
USER root

# Install additional OS packages
RUN apt update && apt upgrade -y && export DEBIAN_FRONTEND=noninteractive \
    && apt -y install --no-install-recommends \
    curl \
    git \
    valkey-tools \
    && apt clean -y \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js (latest LTS)
RUN cd /tmp; curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt install -y nodejs \
    && npm install -g npm@latest

# Install Claude Code
RUN npm install -g @anthropic-ai/claude-code

# Install Go tools
RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install github.com/air-verse/air@latest

# Set up the workspace
WORKDIR /workspace
