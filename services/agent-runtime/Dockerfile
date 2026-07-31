# services/agent-runtime/Dockerfile
FROM python:3.11-slim

WORKDIR /app

# Install Python dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application
COPY . .

ENV PYTHONUNBUFFERED=1
CMD ["python", "-m", "main"]
