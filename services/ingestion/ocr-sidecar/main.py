# services/ingestion/ocr-sidecar/main.py
from fastapi import FastAPI, File, UploadFile, HTTPException
from fastapi.responses import JSONResponse
from doctr.io import DocumentFile
from doctr.models import ocr_predictor
import uvicorn
import os
import tempfile
import boto3
from typing import List, Dict, Any

app = FastAPI(title="docTR OCR Sidecar")

# Initialize docTR predictor
predictor = ocr_predictor(pretrained=True, detect_language=True)

# S3/MinIO client for downloading documents
s3_endpoint = os.getenv("S3_ENDPOINT", "http://minio:9000")
s3_bucket = os.getenv("S3_BUCKET", "ai-auditor")
s3_access_key = os.getenv("S3_ACCESS_KEY", "minioadmin")
s3_secret_key = os.getenv("S3_SECRET_KEY", "minioadmin")

s3_client = boto3.client(
    's3',
    endpoint_url=s3_endpoint,
    aws_access_key_id=s3_access_key,
    aws_secret_access_key=s3_secret_key,
)

@app.get("/health")
async def health():
    return {"status": "healthy"}

@app.post("/ocr/process")
async def process_document(request: Dict[str, Any]):
    """
    Process a document from S3/MinIO and return OCR results with bounding boxes.

    Request body:
    {
        "storage_key": "path/to/document.pdf",
        "doc_type": "invoice|bank_statement|gl_export"
    }
    """
    storage_key = request.get("storage_key")
    doc_type = request.get("doc_type", "invoice")

    if not storage_key:
        raise HTTPException(status_code=400, detail="storage_key required")

    # Download from S3/MinIO
    with tempfile.NamedTemporaryFile(delete=False, suffix=os.path.splitext(storage_key)[1]) as tmp:
        try:
            s3_client.download_fileobj(s3_bucket, storage_key, tmp)
            tmp_path = tmp.name
        except Exception as e:
            raise HTTPException(status_code=404, detail=f"Document not found: {e}")

    try:
        # Process with docTR
        if storage_key.lower().endswith('.pdf'):
            doc = DocumentFile.from_pdf(tmp_path)
        else:
            doc = DocumentFile.from_images(tmp_path)

        result = predictor(doc)

        # Convert to our response format
        pages = []
        for page_idx, page in enumerate(result.pages):
            page_data = {
                "page_number": page_idx + 1,
                "width": page.dimensions[1],  # docTR uses (height, width)
                "height": page.dimensions[0],
                "blocks": []
            }

            for block in page.blocks:
                block_data = {
                    "geometry": block.geometry.tolist(),  # 4 corners [[x1,y1], [x2,y2], [x3,y3], [x4,y4]]
                    "confidence": float(block.confidence),
                    "lines": []
                }

                for line in block.lines:
                    line_data = {
                        "geometry": line.geometry.tolist(),
                        "confidence": float(line.confidence),
                        "words": []
                    }

                    for word in line.words:
                        line_data["words"].append({
                            "value": word.value,
                            "confidence": float(word.confidence),
                            "geometry": word.geometry.tolist(),
                        })

                    block_data["lines"].append(line_data)

                page_data["blocks"].append(block_data)

            pages.append(page_data)

        return JSONResponse({
            "pages": pages,
            "doc_type": doc_type
        })
    finally:
        os.unlink(tmp_path)

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8000)