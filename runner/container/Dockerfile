# The runner image (Apple container, `container build -f runner/container/Dockerfile runner/`).
# The build context is runner/: package.json, package-lock.json and the compiled dist/ (milestone M1).
# TODO: pin the base image by digest once the image is built on a real machine.
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates netcat-openbsd \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin rubi \
 && mkdir -p /opt/rubi-runner /work \
 && chown rubi /work

WORKDIR /opt/rubi-runner
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY dist/ ./dist/

# git asks Rubi Home for credentials through the runner (socket served by the runner, see
# docs/design/rubi-home.md); never a token on disk in the image.
COPY container/git-credential-rubi /usr/local/bin/git-credential-rubi
RUN chmod 0755 /usr/local/bin/git-credential-rubi \
 && git config --system credential.helper /usr/local/bin/git-credential-rubi \
 && git config --system credential.useHttpPath true

# Telemetry stays off; the proxy and the run token come from the supervisor's environment at exec time.
ENV DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    HOME=/tmp CLAUDE_CONFIG_DIR=/work/.rubi/claude

USER 10001
WORKDIR /work
# The supervisor starts the container idle and execs `node /opt/rubi-runner/dist/main.js` into it.
ENTRYPOINT ["sleep", "infinity"]
