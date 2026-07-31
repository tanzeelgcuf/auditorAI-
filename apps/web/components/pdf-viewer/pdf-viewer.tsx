"use client";

import { useEffect, useRef, useState } from "react";
import { CitationOverlay, CitationTarget } from "./citation-overlay";

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

  useEffect(() => {
    let cancelled = false;
    let doc: PDFDocumentProxy | null = null;

    async function load() {
      try {
        const pdfjs = await import("pdfjs-dist");
        pdfjs.GlobalWorkerOptions.workerSrc = `//cdnjs.cloudflare.com/ajax/libs/pdf.js/${pdfjs.version}/pdf.worker.min.js`;
        const loaded = await pdfjs.getDocument({ url, disableAutoFetch: true }).promise;
        if (cancelled) return;
        doc = loaded;
        setPdf(loaded);
        setPageNum(1);
      } catch (e) {
        setError(e instanceof Error ? e.message : "Failed to load PDF");
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

  return (
    <div className="relative w-full overflow-hidden rounded-lg border border-slate-200 bg-slate-100">
      {error ? (
        <div className="flex h-64 items-center justify-center text-sm text-red-600">{error}</div>
      ) : (
        <>
          <canvas ref={canvasRef} className="mx-auto max-w-full" />
          {pdf && pdf.numPages > 1 && (
            <div className="absolute bottom-3 left-1/2 flex -translate-x-1/2 items-center gap-2 rounded-md bg-white/90 px-2 py-1 text-xs shadow">
              <button onClick={() => setScale((s) => Math.max(0.5, s - 0.2))} aria-label="Zoom out">−</button>
              <span>Page {pageNum}/{pdf.numPages}</span>
              <button onClick={() => setScale((s) => Math.min(3, s + 0.2))} aria-label="Zoom in">+</button>
            </div>
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
