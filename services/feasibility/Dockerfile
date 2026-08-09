# feasibility — the Python worker (#166). Same shape as the Go pattern in
# services/tasking-api/Dockerfile — repo-root context, one service directory
# copied in, one process at the end — with uv standing where the Go toolchain
# stands, because uv.lock is the resolution the tests ran against and
# reproducing it by hand is how a container ends up on different libraries
# than CI (the same reasoning as the integration harness's `uv sync --frozen`).
#
# No multi-stage: the worker imports its dependencies at runtime, so there is
# no smaller artifact to ship than the synced environment itself.
FROM ghcr.io/astral-sh/uv:python3.12-bookworm
WORKDIR /app
COPY services/feasibility /app
# --frozen: the lockfile is the authority, and a container that resolved
# something newer would be the drift this file exists to prevent.
# --no-dev: pytest and friends have no business in a running worker.
RUN uv sync --frozen --no-dev
# --no-sync so startup never reaches for the network; the environment was
# built above or it does not exist.
ENTRYPOINT ["uv", "run", "--no-sync", "python", "-m", "feasibility"]
