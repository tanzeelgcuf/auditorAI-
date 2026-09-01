"use client";

import { cn } from "../../lib/utils";
import { motion } from "framer-motion";

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
    <motion.div
      initial={{ opacity: 0, scale: 0.95 }}
      animate={{ opacity: 1, scale: 1 }}
      exit={{ opacity: 0, scale: 0.95 }}
      transition={{ duration: 0.2, ease: [0.4, 0, 0.2, 1] }}
      className="pointer-events-none absolute inset-0"
    >
      <motion.div
        initial={{ boxShadow: "0 0 0 0px rgb(249 115 22 / 0)" }}
        animate={{ boxShadow: "0 0 0 4px rgb(249 115 22 / 0.3)" }}
        transition={{ duration: 1.5, repeat: Infinity, ease: "easeInOut" }}
        className={cn(
          "pointer-events-auto absolute border-2 cursor-pointer",
          "border-amber-500 bg-amber-400/30 transition-all duration-200",
          "hover:bg-amber-400/50 hover:border-amber-400",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-500 focus-visible:ring-offset-2",
        )}
        style={{
          left: `${active.bbox.x * 100}%`,
          top: `${active.bbox.y * 100}%`,
          width: `${active.bbox.width * 100}%`,
          height: `${active.bbox.height * 100}%`,
        }}
        onClick={() => onSelect(active)}
        role="button"
        tabIndex={0}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onSelect(active);
          }
        }}
        aria-label={`Citation for finding ${active.findingId}`}
        title="Source region for this finding — click to select"
      />
      <div
        className="absolute -top-6 left-0 px-2 py-0.5 text-xs font-medium text-amber-900 bg-amber-400 rounded"
        style={{ left: `${active.bbox.x * 100}%`, top: `${active.bbox.y * 100}%` }}
      >
        Source
      </div>
    </motion.div>
  );
}