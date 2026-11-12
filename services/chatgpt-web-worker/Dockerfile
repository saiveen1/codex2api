FROM ghcr.io/astral-sh/uv:0.11.25 AS uv
FROM python:3.13.7-slim-bookworm
COPY --from=uv /uv /usr/local/bin/uv
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 UV_LINK_MODE=copy \
    CHATGPT_WEB_STATE_DIR=/state PATH=/app/.venv/bin:$PATH
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv sync --locked --no-dev --no-managed-python && \
    groupadd --gid 10001 worker && useradd --uid 10001 --gid worker --no-create-home worker && \
    mkdir /state && chown worker:worker /state && chmod 700 /state
COPY app.py backend.py contract.py conversation.py executor.py ledger.py proof.py transport.py ./
COPY LICENSE NOTICE PROVENANCE.md README.md ./
USER worker
CMD ["uvicorn", "app:create_app", "--factory", "--host", "127.0.0.1", "--port", "8091", "--no-access-log"]
