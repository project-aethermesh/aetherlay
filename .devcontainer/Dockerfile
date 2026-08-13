FROM dhi.io/golang:1.26-dev

# Run as root
USER root

# Install additional OS packages
RUN apt update && apt upgrade -y && export DEBIAN_FRONTEND=noninteractive \
    && getent group adm >/dev/null || groupadd -r -g 4 adm \
    && apt -y install --no-install-recommends \
    bsdutils \
    curl \
    git \
    libc-bin \
    unzip \
    valkey-tools \
    && ldconfig \
    && apt clean -y \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js (latest LTS)
RUN cd /tmp; curl -fsSL https://deb.nodesource.com/setup_lts.x | bash - \
    && apt install -y nodejs \
    && npm install -g npm@latest

# Install Claude Code
RUN curl -fsSL https://claude.ai/install.sh | bash -s stable \
    && echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc \
    && echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bash_profile

# Install Go tools
RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install github.com/air-verse/air@latest

# Set up the workspace
WORKDIR /workspace

SHELL ["/bin/bash", "-c"]
