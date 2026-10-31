# The runner image (Apple container, `container build -f runner/container/Dockerfile runner/`).
# The build context is runner/: package*.json, tsconfig*.json, src/ and container/ (the same files the Rubi Home
# binary embeds, runner/embed.go); the runner is compiled here, in the builder stage.
# The base image is pinned by the digest of its multi-platform index (node 22.23.4, bookworm, 2026-10-28);
# update both together.
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:4b150b48b4116e2a22f2c39cc27025fbf4f153e2078fd3b14d159fdb5c7279da

# Builder: the packages from the lockfile (with dev dependencies, no install scripts) and the compiled runner.
FROM ${NODE_IMAGE} AS builder
WORKDIR /build
COPY package.json package-lock.json tsconfig.json tsconfig.build.json ./
RUN npm ci --ignore-scripts
COPY src/ ./src/
RUN npm run build

FROM ${NODE_IMAGE}

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates netcat-openbsd \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin rubi \
 && mkdir -p /opt/rubi-runner /work \
 && chown rubi /work

WORKDIR /opt/rubi-runner
COPY package.json package-lock.json ./
# --ignore-scripts: no package's install script runs during the build.
RUN npm ci --omit=dev --ignore-scripts
COPY --from=builder /build/dist/ ./dist/

# git asks Rubi Home for credentials through the runner (socket served by the runner, see
# docs/design/rubi-home.md); never a token on disk in the image.
COPY container/git-credential-rubi /usr/local/bin/git-credential-rubi
# The sign-in helper of the setup wizard (it runs `claude setup-token` under `script`, from util-linux); it is
# run in the short-lived auth container, never by the runner.
COPY container/rubi-claude-token /usr/local/bin/rubi-claude-token
# `ssh <target> <command>`: no ssh client is installed; this wrapper speaks to the runner's socket (RUBI_SSH_SOCK) and Rubi
# Home is the SSH client (docs/design/agent-roles.md section 9). It is also found as `ssh`, first in PATH. Git over
# ssh is therefore not supported (git would call the wrapper, which refuses options); remotes are https, with the
# credential helper above.
COPY container/rubi-ssh /usr/local/bin/rubi-ssh
RUN chmod 0755 /usr/local/bin/git-credential-rubi /usr/local/bin/rubi-claude-token /usr/local/bin/rubi-ssh \
 && mkdir -p /usr/local/rubi/bin \
 && ln -s /usr/local/bin/rubi-ssh /usr/local/rubi/bin/ssh \
 && git config --system credential.helper /usr/local/bin/git-credential-rubi \
 && git config --system credential.useHttpPath true \
 && git config --system protocol.ssh.allow never
ENV PATH=/usr/local/rubi/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# Telemetry stays off; the proxy and the run token come from the supervisor's environment at exec time.
ENV DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    HOME=/tmp CLAUDE_CONFIG_DIR=/work/.rubi/claude

USER 10001
WORKDIR /work
# The supervisor starts the container idle and execs `node /opt/rubi-runner/dist/main.js` into it.
ENTRYPOINT ["sleep", "infinity"]
