ARG DEBIAN_VERSION=bookworm-slim

FROM debian:${DEBIAN_VERSION} AS base-node22

ARG NODE_MAJOR=22
ARG USER_NAME=pi
ARG USER_UID=1000
ARG USER_GID=1000

ENV DEBIAN_FRONTEND=noninteractive
ENV HOME=/home/${USER_NAME}
ENV NPM_CONFIG_CACHE=/tmp/npm-cache

RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    gnupg \
  && install -d -m 0755 /etc/apt/keyrings \
  && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
    | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
  && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
    > /etc/apt/sources.list.d/nodesource.list \
  && apt-get update \
  && apt-get install -y --no-install-recommends \
    nodejs \
    bash \
    zsh \
    git \
    git-lfs \
    openssh-client \
    rsync \
    jq \
    ripgrep \
    fd-find \
    tree \
    file \
    less \
    vim-tiny \
    nano \
    procps \
    lsof \
    psmisc \
    iproute2 \
    dnsutils \
    netcat-openbsd \
    unzip \
    zip \
    xz-utils \
    tar \
    gzip \
    bzip2 \
    tzdata \
    locales \
  && corepack enable \
  && groupadd --gid "${USER_GID}" "${USER_NAME}" \
  && useradd --uid "${USER_UID}" --gid "${USER_GID}" --create-home --shell /bin/bash "${USER_NAME}" \
  && mkdir -p "${HOME}/.pi/agent" "${HOME}/.pi/pico/agent" "${HOME}/.pi/pico/sessions/docker" "${HOME}/.agents/skills" /workspace \
  && chmod -R 0777 "${HOME}" /workspace \
  && ln -s /usr/bin/fdfind /usr/local/bin/fd \
  && npm install -g npm@latest \
  && npm cache clean --force \
  && apt-get clean \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

FROM base-node22 AS pi-agent

ARG PI_NPM_PACKAGE=@earendil-works/pi-coding-agent@latest

RUN npm install -g "${PI_NPM_PACKAGE}" \
  && npm cache clean --force \
  && rm -rf /home/${USER_NAME}/.npm /tmp/npm-cache

USER ${USER_NAME}
WORKDIR /workspace

CMD ["pi", "--help"]
