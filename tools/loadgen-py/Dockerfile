FROM python:3.12-slim
RUN pip install --no-cache-dir httpx pyjwt
WORKDIR /app
COPY burst.py .
ENTRYPOINT ["python3", "burst.py"]
