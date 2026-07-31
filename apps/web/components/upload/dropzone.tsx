"use client";

import { useCallback, useRef, useState } from "react";
import { cn } from "../../lib/utils";

const ACCEPTED = [".pdf", ".png", ".jpg", ".jpeg", ".csv", ".xlsx", ".ofx", ".qfx"];
const MAX_SIZE = 25 * 1024 * 1024; // 25MB per doc 06 §5

interface DropzoneProps {
  onFiles: (files: File[]) => void;
  uploading?: boolean;
}

export function Dropzone({ onFiles, uploading }: DropzoneProps) {
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const validate = useCallback((files: File[]) => {
    setError(null);
    const valid: File[] = [];
    for (const f of files) {
      const ext = "." + f.name.split(".").pop()?.toLowerCase();
      if (!ACCEPTED.includes(ext)) {
        setError(`Unsupported file type: ${f.name}. Accepted: PDF, PNG, JPG, CSV, XLSX, OFX/QFX.`);
        continue;
      }
      if (f.size > MAX_SIZE) {
        setError(`File too large: ${f.name} (max 25MB).`);
        continue;
      }
      valid.push(f);
    }
    if (valid.length > 0) onFiles(valid);
  }, [onFiles]);

  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center rounded-lg border-2 border-dashed p-12 text-center transition-colors",
        dragging ? "border-slate-900 bg-slate-50" : "border-slate-300",
        uploading && "opacity-50",
      )}
      onDragOver={(e) => {
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
        validate(Array.from(e.dataTransfer.files));
      }}
      onClick={() => inputRef.current?.click()}
      role="button"
      aria-label="Upload documents"
    >
      <input
        ref={inputRef}
        type="file"
        multiple
        accept={ACCEPTED.join(",")}
        className="hidden"
        onChange={(e) => {
          if (e.target.files) validate(Array.from(e.target.files));
          e.target.value = "";
        }}
      />
      <div className="text-3xl mb-2">⬆</div>
      <p className="font-medium text-slate-700">
        {uploading ? "Uploading…" : "Drag & drop documents here"}
      </p>
      <p className="mt-1 text-sm text-slate-500">
        or click to browse · PDF, PNG, JPG, CSV, XLSX, OFX/QFX · max 25MB
      </p>
      {error && <p className="mt-3 text-sm text-red-600">{error}</p>}
    </div>
  );
}
