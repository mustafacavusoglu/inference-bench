# Stage 1: build the ShareGPT prompt set (set SKIP_SHAREGPT=1 for offline builds)
FROM registry.access.redhat.com/ubi9/python-311 AS data
ARG SKIP_SHAREGPT=0
COPY prepare_sharegpt.py /tmp/prepare_sharegpt.py
RUN if [ "$SKIP_SHAREGPT" = "1" ]; then : > /tmp/sharegpt.jsonl; \
    else python /tmp/prepare_sharegpt.py - /tmp/sharegpt.jsonl; fi

# Stage 2: runtime
FROM registry.access.redhat.com/ubi9/python-311
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY *.py ./
COPY static/ static/
USER 0
RUN mkdir -p /app/data /data && chgrp -R 0 /app /data && chmod -R g=u /app /data
COPY --from=data /tmp/sharegpt.jsonl /app/data/sharegpt.jsonl
ENV DATA_DIR=/data SHAREGPT_PATH=/app/data/sharegpt.jsonl
EXPOSE 8080
USER 1001
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080"]
