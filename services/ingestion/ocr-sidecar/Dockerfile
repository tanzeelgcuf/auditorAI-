# services/ingestion/ocr-sidecar/Dockerfile
FROM python:3.11-slim

WORKDIR /app

# Install system dependencies for docTR
RUN apt-get update && apt-get install -y \
    libgl1-mesa-glx \
    libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application
COPY main.py .

EXPOSE 8000

CMD ["python", "main.py"]