FROM mcr.microsoft.com/devcontainers/go:1.24

# Run as root
USER root

# Install additional OS packages
RUN apt update && apt upgrade -y && export DEBIAN_FRONTEND=noninteractive \
    && apt -y install --no-install-recommends \
    curl \
    git \
    redis-tools \
    nodejs \
    npm \
    && apt clean -y \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code
RUN npm install -g @anthropic-ai/claude-code

# Install Go tools
RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install github.com/air-verse/air@latest

# Set up the workspace
WORKDIR /workspace

# Enable bash completion
RUN . /etc/bash_completion