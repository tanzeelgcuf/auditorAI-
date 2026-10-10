# services/ingestion/ocr-sidecar/Dockerfile
FROM python:3.11-slim

WORKDIR /app

# Install system dependencies for docTR
#
# `libgl1`, not `libgl1-mesa-glx`. python:3.11-slim is Debian bookworm-based, and
# bookworm dropped the mesa-glx package name — the libGL.so.1 provider there is
# `libgl1`. `apt-get install libgl1-mesa-glx` on bookworm ends the RUN layer with
# "E: Unable to locate package", so this Dockerfile could not build. `libgl1`
# also exists on bullseye, so this name is correct either way rather than trading
# one base-image assumption for another.
#
# What needs it: python-doctr[torch] pulls opencv-python, whose cv2 links
# libGL.so.1 at import. REASONED, not observed — there is no docker in the
# environment this was changed in, so the build is unproven.
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgl1 \
    libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*

# Install Python dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application
COPY main.py .

EXPOSE 8000

CMD ["python", "main.py"]