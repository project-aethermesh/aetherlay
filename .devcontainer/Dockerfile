FROM dhi.io/golang:1.26-dev

# Run as root
USER root

SHELL ["/bin/bash", "-c"]

# Install additional OS packages
RUN apt update && apt upgrade -y && export DEBIAN_FRONTEND=noninteractive \
    && apt -y install --no-install-recommends \
    bsdutils \
    curl \
    git \
    libc-bin \
    locales \
    unzip \
    && ldconfig \
    && apt clean -y \
    && rm -rf /var/lib/apt/lists/*

# Generate and set the locale used by the shell
RUN sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen \
    && locale-gen

ENV LANG=en_US.UTF-8
ENV LANGUAGE=en_US:en
ENV LC_ALL=en_US.UTF-8

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

# Install Valkey's CLI
RUN getent group adm >/dev/null || groupadd -r -g 4 adm \
    && apt install -y valkey-tools

# Set up the workspace
WORKDIR /workspace
