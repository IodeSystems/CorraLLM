# What corrallm needs on a machine that has NOTHING.
#
# This file is the answer to "what does this actually depend on", and it is only
# trustworthy because it starts from a bare image and fails when the answer is
# wrong. The build context is `git archive HEAD`, so what is tested is what is
# COMMITTED — not the working tree, and notably not go.work, which is gitignored
# and would otherwise let the container borrow this machine's neighbours the
# same way the local build did.
#
# Every package below says WHY. A list without reasons rots into a list nobody
# dares trim.
FROM ubuntu:24.04

#   ca-certificates — TLS for module downloads; without it every fetch fails
#                     with an unhelpful x509 error
#   git             — the module system shells out to it to resolve VCS deps
#   make            — so the container runs the SAME target you run locally,
#                     rather than a second copy of the commands that can drift
#   tar             — unpacks the Go toolchain below
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git make tar \
 && rm -rf /var/lib/apt/lists/*

# The Go toolchain is NOT apt's: Ubuntu ships a version older than go.mod's
# `go` directive, and the version is passed in from the Makefile so this cannot
# drift from what the module declares.
ARG GO_VERSION
ARG GO_SHA256
ADD https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz /tmp/go.tgz
RUN if [ -n "$GO_SHA256" ]; then echo "$GO_SHA256  /tmp/go.tgz" | sha256sum -c -; fi \
 && tar -C /usr/local -xzf /tmp/go.tgz \
 && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH

WORKDIR /src
COPY . .

# GOFLAGS=-mod=mod is deliberately NOT set: a build that needs to edit go.mod is
# a build whose dependencies were not pinned, which is the thing being proven.
RUN go build ./...

# The same isolation checks the Makefile runs, now on a machine that has no
# ../gwag to fall back on.
RUN make verify-deps VERIFY_DOCKER=0

# Tests, because "it compiles" is a weaker claim than anyone wants from this.
# internal/proc spawns real process groups and is the slowest by far; it is
# included precisely because that is the code most likely to assume a
# developer's machine.
RUN go test ./...

RUN echo "corrallm builds, verifies and tests from a bare ubuntu:24.04"
