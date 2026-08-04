# OCR Sidecar (docTR)

Runs docTR OCR for PDF/image documents. Consumes `storage_key` + `doc_type`,
downloads from MinIO/S3, returns OCR blocks with bounding boxes.

## Local run (no docker)

Port 8000 is commonly taken by other local apps; use 8001.

```bash
cd services/ingestion/ocr-sidecar

# One-time venv setup (python3.12 — torch has no py3.13 wheels yet)
python3.12 -m venv .venv
.venv/bin/pip install torch fastapi uvicorn boto3 python-multipart
.venv/bin/pip install python-doctr   # NOT 'doctr' (that's a docs-deploy tool)
.venv/bin/pip install 'numpy<2' 'scipy>=1.11,<1.14'  # doctr needs old numpy/scipy

# Run
export S3_ENDPOINT=http://localhost:9000 S3_BUCKET=ai-auditor \
  S3_ACCESS_KEY=minioadmin S3_SECRET_KEY=minioadmin
.venv/bin/python -m uvicorn main:app --host 0.0.0.0 --port 8001
```

Health: `curl localhost:8001/health` → `{"status":"healthy"}`

## Why 8001

The docker-compose runs the sidecar on :8000. On a dev Mac another app
(IG_Automation) occupies :8000, so run locally on :8001 and point ingestion at
it: `OCR_SIDECAR_URL=http://localhost:8001`.

## Dep pin notes

- `torch==2.2.2` (CPU) — newer needs py3.13 wheels.
- `numpy<2` + `scipy<1.14` — doctr 0.8.x's scipy path uses `np.long`, removed
  in numpy 2.x.
