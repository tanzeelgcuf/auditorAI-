"use client";

import { useCallback, useRef, useState } from "react";
import { cn } from "../../lib/utils";
import { motion } from "framer-motion";
import { Upload, Loader2, AlertCircle, FileText, Image, FileSpreadsheet } from "lucide-react";

const ACCEPTED = [".pdf", ".png", ".jpg", ".jpeg", ".csv", ".xlsx", ".ofx", ".qfx"];
// 25MB, and it must stay equal to maxUploadSize in
// services/api/internal/documents/documents.go. This is a client-side pre-check
// only — the server is what actually rejects, so a drift here shows up as an
// upload that the browser accepts and the API 413s.
const MAX_SIZE = 25 * 1024 * 1024;

interface DropzoneProps {
  onFiles: (files: File[]) => void;
  uploading?: boolean;
}

function getFileIcon(ext: string) {
  switch (ext) {
    case ".pdf":
      return <FileText className="h-5 w-5 text-primary" aria-hidden="true" />;
    case ".png":
    case ".jpg":
    case ".jpeg":
      // eslint-disable-next-line jsx-a11y/alt-text
      return <Image className="h-5 w-5 text-primary" aria-hidden="true" />;
    case ".csv":
    case ".xlsx":
      return <FileSpreadsheet className="h-5 w-5 text-primary" aria-hidden="true" />;
    default:
      return <FileText className="h-5 w-5 text-primary" aria-hidden="true" />;
  }
}

export function Dropzone({ onFiles, uploading }: DropzoneProps) {
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [previewFiles, setPreviewFiles] = useState<File[]>([]);
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
    if (valid.length > 0) {
      setPreviewFiles(valid);
      onFiles(valid);
    }
  }, [onFiles]);

  const removeFile = (file: File) => {
    setPreviewFiles((prev) => prev.filter((f) => f !== file));
  };

  const clearAll = () => {
    setPreviewFiles([]);
    setError(null);
    if (inputRef.current) inputRef.current.value = "";
  };

  return (
    <div className="w-full">
      <motion.div
        initial={{ opacity: 0, y: 20 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.3, ease: [0.4, 0, 0.2, 1] }}
        className={cn(
          "flex flex-col items-center justify-center rounded-lg border-2 border-dashed p-8 text-center transition-all duration-200",
          dragging
            ? "border-primary bg-primary/5"
            : "border-border hover:border-primary/50",
          uploading && "opacity-60 pointer-events-none",
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
        onClick={() => !uploading && inputRef.current?.click()}
        role="button"
        aria-label="Upload documents"
        whileTap={{ scale: 0.98 }}
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
        {uploading ? (
          <motion.div
            initial={{ opacity: 0, scale: 0.9 }}
            animate={{ opacity: 1, scale: 1 }}
            transition={{ duration: 0.2 }}
            className="flex flex-col items-center gap-3"
          >
            <Loader2 className="h-8 w-8 animate-spin text-primary" aria-hidden="true" />
            <p className="font-medium text-foreground">Uploading…</p>
            <p className="text-sm text-muted-foreground">Please wait while files are processed</p>
          </motion.div>
        ) : previewFiles.length > 0 ? (
          <motion.div
            initial={{ opacity: 0, y: 10 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ duration: 0.2 }}
            className="w-full max-w-md space-y-2"
          >
            <div className="flex items-center justify-between">
              <p className="font-medium text-foreground">Files ready to upload</p>
              <button
                onClick={(e) => {
                  e.stopPropagation();
                  clearAll();
                }}
                className="text-sm text-muted-foreground hover:text-foreground transition-colors"
              >
                Clear all
              </button>
            </div>
            <div className="space-y-2 max-h-60 overflow-y-auto">
              {previewFiles.map((file) => (
                <motion.div
                  key={file.name}
                  initial={{ opacity: 0, x: -20 }}
                  animate={{ opacity: 1, x: 0 }}
                  transition={{ duration: 0.2 }}
                  className="flex items-center gap-3 p-3 rounded-md border border-border bg-card"
                >
                  <div className="flex-shrink-0">{getFileIcon("." + file.name.split(".").pop()?.toLowerCase() || "")}</div>
                  <div className="flex-1 min-w-0 text-left">
                    <p className="font-medium text-sm truncate text-foreground">{file.name}</p>
                    <p className="text-xs text-muted-foreground">{(file.size / 1024 / 1024).toFixed(1)} MB</p>
                  </div>
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      removeFile(file);
                    }}
                    className="flex-shrink-0 p-1 rounded hover:bg-muted transition-colors"
                    aria-label={`Remove ${file.name}`}
                  >
                    <AlertCircle className="h-4 w-4 text-muted-foreground hover:text-destructive" aria-hidden="true" />
                  </button>
                </motion.div>
              ))}
            </div>
          </motion.div>
        ) : (
          <motion.div
            initial={{ opacity: 0, y: 10 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ duration: 0.3, delay: 0.1 }}
            className="flex flex-col items-center gap-3"
          >
            <motion.div
              animate={{ rotate: dragging ? 180 : 0 }}
              transition={{ duration: 0.3, ease: [0.4, 0, 0.2, 1] }}
              className="p-3 rounded-full bg-primary/10 text-primary"
              whileHover={{ scale: 1.1 }}
            >
              <Upload className="h-6 w-6" aria-hidden="true" />
            </motion.div>
            <p className="font-medium text-foreground">Drag & drop documents here</p>
            <p className="text-sm text-muted-foreground">or click to browse</p>
            <div className="flex flex-wrap items-center justify-center gap-2 text-xs text-muted-foreground mt-1">
              <span className="px-2 py-0.5 rounded bg-muted">PDF</span>
              <span className="px-2 py-0.5 rounded bg-muted">PNG</span>
              <span className="px-2 py-0.5 rounded bg-muted">JPG</span>
              <span className="px-2 py-0.5 rounded bg-muted">CSV</span>
              <span className="px-2 py-0.5 rounded bg-muted">XLSX</span>
              <span className="px-2 py-0.5 rounded bg-muted">OFX/QFX</span>
              <span className="px-2 py-0.5 rounded bg-muted">Max 25MB</span>
            </div>
          </motion.div>
        )}
        {error && (
          <motion.div
            initial={{ opacity: 0, y: -10 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -10 }}
            transition={{ duration: 0.2 }}
            className="mt-4 w-full max-w-md flex items-center gap-2 rounded-md border border-destructive/20 bg-destructive-bg p-3 text-sm text-destructive"
          >
            <AlertCircle className="h-4 w-4 flex-shrink-0" aria-hidden="true" />
            {error}
          </motion.div>
        )}
      </motion.div>
    </div>
  );
}