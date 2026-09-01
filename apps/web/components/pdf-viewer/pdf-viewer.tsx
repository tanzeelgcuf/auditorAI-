"use client";

import { useEffect, useRef, useState } from "react";
import { CitationOverlay, CitationTarget } from "./citation-overlay";
import { Loader2, Minus, Plus, RotateCw, Download } from "lucide-react";
import { MotionDiv, MotionButton } from "../ui/motion";

interface PdfViewerProps {
  url: string;
  citation: CitationTarget | null;
  onSelectCitation: (t: CitationTarget) => void;
}

/**
 * Renders a PDF with clickable citation highlights.
 * pdfjs-dist is dynamically imported (it's a Web Worker-heavy lib).
 */
import type { PDFDocumentProxy } from "pdfjs-dist";

export function PdfViewer({ url, citation, onSelectCitation }: PdfViewerProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [pdf, setPdf] = useState<PDFDocumentProxy | null>(null);
  const [pageNum, setPageNum] = useState(1);
  const [scale, setScale] = useState(1.4);
  const [error, setError] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    let doc: PDFDocumentProxy | null = null;

    async function load() {
      setIsLoading(true);
      setError(null);
      try {
        const pdfjs = await import("pdfjs-dist");
        pdfjs.GlobalWorkerOptions.workerSrc = `//cdnjs.cloudflare.com/ajax/libs/pdf.js/${pdfjs.version}/pdf.worker.min.js`;
        const loaded = await pdfjs.getDocument({ url, disableAutoFetch: true }).promise;
        if (cancelled) return;
        doc = loaded;
        setPdf(loaded);
        setPageNum(1);
        setIsLoading(false);
      } catch (e) {
        setError(e instanceof Error ? e.message : "Failed to load PDF");
        setIsLoading(false);
      }
    }
    load();
    return () => {
      cancelled = true;
      doc?.destroy();
    };
  }, [url]);

  useEffect(() => {
    if (!pdf) return;
    let cancelled = false;
    (async () => {
      const p = await pdf.getPage(pageNum);
      if (cancelled) return;
      const viewport = p.getViewport({ scale });
      const canvas = canvasRef.current;
      if (!canvas) return;
      const ctx = canvas.getContext("2d");
      if (!ctx) return;
      canvas.width = viewport.width;
      canvas.height = viewport.height;
      await p.render({ canvasContext: ctx, viewport, canvas }).promise;
    })();
    return () => {
      cancelled = true;
    };
  }, [pdf, pageNum, scale]);

  const handleZoomIn = () => setScale((s) => Math.min(3, s + 0.2));
  const handleZoomOut = () => setScale((s) => Math.max(0.5, s - 0.2));
  const handleRotate = () => {
    // Rotation would require more complex canvas manipulation
    // For now, we just reset scale
    setScale(1.4);
  };

  return (
    <div className="relative w-full overflow-hidden rounded-lg border border-border bg-muted">
      {error ? (
        <MotionDiv variant="fadeIn" className="flex h-64 items-center justify-center text-sm text-destructive">
          {error}
        </MotionDiv>
      ) : isLoading ? (
        <div className="flex h-64 items-center justify-center">
          <Loader2 className="h-8 w-8 animate-spin text-primary" aria-hidden="true" />
          <span className="sr-only">Loading PDF…</span>
        </div>
      ) : (
        <>
          <canvas ref={canvasRef} className="mx-auto max-w-full" />
          {pdf && pdf.numPages > 1 && (
            <MotionDiv variant="slideUp" className="absolute bottom-3 left-1/2 flex -translate-x-1/2 items-center gap-2 rounded-lg bg-card/95 px-3 py-2 text-xs shadow-lg border border-border backdrop-blur supports-[backdrop-filter]:bg-card/80">
              <MotionButton
                variant="ghost"
                size="icon"
                onClick={handleZoomOut}
                aria-label="Zoom out"
                className="h-8 w-8"
              >
                <Minus className="h-4 w-4" aria-hidden="true" />
              </MotionButton>
              <span className="px-2 font-mono text-foreground">{pageNum}/{pdf.numPages}</span>
              <MotionButton
                variant="ghost"
                size="icon"
                onClick={handleZoomIn}
                aria-label="Zoom in"
                className="h-8 w-8"
              >
                <Plus className="h-4 w-4" aria-hidden="true" />
              </MotionButton>
              <div className="w-px h-6 bg-border mx-1" />
              <MotionButton
                variant="ghost"
                size="icon"
                onClick={handleRotate}
                aria-label="Reset view"
                className="h-8 w-8"
              >
                <RotateCw className="h-4 w-4" aria-hidden="true" />
              </MotionButton>
              <a
                href={url}
                download
                className="h-8 w-8 flex items-center justify-center text-muted-foreground hover:text-foreground transition-colors"
              >
                <Download className="h-4 w-4" aria-hidden="true" />
              </a>
            </MotionDiv>
          )}
          <CitationOverlay
            active={citation}
            onSelect={(t) => onSelectCitation(t)}
          />
        </>
      )}
    </div>
  );
}