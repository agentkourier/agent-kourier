# syntax=docker/dockerfile:1
# The recorder of the docs' clips: vhs (charmbracelet), which drives a shell in a headless browser and writes
# the GIF, plus the kubectl, helm and vim the clip types, and gifsicle, which appends the Slack still. The base
# is pinned by digest and the two binaries by version and SHA-256; vim and gifsicle come unpinned from the base's
# Debian release, so they can differ between builds. record.sh builds it as agent-kourier-clip-vhs:dev; nothing
# pushes it.
#
# kubectl matches the kind node of hack/kind/lib.sh (NODE_IMAGE), and helm is the version CI pins
# (.github/workflows/ci.yml, azure/setup-helm). Renovate does not see these URLs: bump them by hand with those two.

FROM ghcr.io/charmbracelet/vhs:v0.12.1@sha256:ea49a6a1c529be83153e88321892b5585964418f0b9055e8c1e0d732194234a3 AS base
RUN apt-get update \
 && apt-get install -y --no-install-recommends vim gifsicle \
 && rm -rf /var/lib/apt/lists/*

FROM base AS tools-amd64
ADD --checksum=sha256:6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f \
    https://dl.k8s.io/release/v1.37.0/bin/linux/amd64/kubectl /usr/local/bin/kubectl
ADD --checksum=sha256:0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b \
    https://get.helm.sh/helm-v4.3.1-linux-amd64.tar.gz /tmp/helm.tar.gz

FROM base AS tools-arm64
ADD --checksum=sha256:922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3 \
    https://dl.k8s.io/release/v1.37.0/bin/linux/arm64/kubectl /usr/local/bin/kubectl
ADD --checksum=sha256:8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88 \
    https://get.helm.sh/helm-v4.3.1-linux-arm64.tar.gz /tmp/helm.tar.gz

FROM tools-${TARGETARCH}
RUN chmod 0755 /usr/local/bin/kubectl \
 && tar -xzf /tmp/helm.tar.gz -C /usr/local/bin --strip-components=1 --wildcards '*/helm' \
 && rm /tmp/helm.tar.gz
# Colours on, line numbers, and every match of a search highlighted, so the viewer sees the lines marked "yours".
RUN printf '%s\n' 'syntax on' 'set number' 'set hlsearch' 'set laststatus=0' 'set shortmess+=I' >/root/.vimrc
