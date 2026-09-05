# Builds the official EURO/ROADEF 2026 checker (v1.2.2, static) in a container.
# The checker source lives in checker/src and only needs the networktools headers.
#
# Build (from repo root):  docker build -f scripts/docker_checker.Dockerfile -t tasr-checker .
# Run: docker run --rm -v "<abs-repo-root>:/data" tasr-checker \
#        --net  /data/setA/setA-XX-net.json  --tm /data/setA/setA-XX-tm.json \
#        --scenario /data/setA/setA-XX-scenario.json --srpaths /data/runs/setA-XX-srpaths.json
FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates build-essential \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 https://gitlab.com/Orange-OpenSource/network-optimization-tools/networktools.git /networktools
COPY checker/src /cs
RUN make -C /cs IDIRNT=/networktools/networktools \
    && cp /cs/checker-v1.2.2-x86-64_linux /usr/local/bin/checker

FROM ubuntu:24.04
COPY --from=build /usr/local/bin/checker /usr/local/bin/checker
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/checker"]
