# Demo filesystem connector. Pure stdlib, no requirements.txt needed.
FROM python:3.12-alpine
WORKDIR /app
COPY connector.py /app/connector.py
RUN adduser -D -H -u 65532 connector
USER 65532
ENTRYPOINT ["python", "/app/connector.py"]
