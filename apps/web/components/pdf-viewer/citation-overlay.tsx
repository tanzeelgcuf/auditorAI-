"use client";

import { cn } from "../../lib/utils";

export interface CitationTarget {
  findingId: string;
  pageNumber: number;
  bbox: { x: number; y: number; width: number; height: number };
}

interface CitationOverlayProps {
  active: CitationTarget | null;
  onSelect: (target: CitationTarget) => void;
}

/**
 * Renders clickable, highlighted bbox regions over a PDF page.
 * The overlay is positioned absolutely over the canvas; normalized 0-1
 * coordinates map directly to percentage positions.
 */
export function CitationOverlay({ active, onSelect }: CitationOverlayProps) {
  if (!active) return null;

  return (
    <div className="pointer-events-none absolute inset-0">
      <div
        className={cn(
          "pointer-events-auto absolute border-2",
          "border-amber-500 bg-amber-400/40 transition-all duration-200",
        )}
        style={{
          left: `${active.bbox.x * 100}%`,
          top: `${active.bbox.y * 100}%`,
          width: `${active.bbox.width * 100}%`,
          height: `${active.bbox.height * 100}%`,
        }}
        onClick={() => onSelect(active)}
        role="button"
        aria-label={`Citation for finding ${active.findingId}`}
        title="Source region for this finding"
      />
    </div>
  );
}
