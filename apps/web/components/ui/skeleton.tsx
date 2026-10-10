"use client";

import { cn } from "../../lib/utils";
import { motion } from "framer-motion";

interface SkeletonProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?: "text" | "circular" | "rectangular" | "card";
  animation?: "pulse" | "wave" | "none";
}

export function Skeleton({
  className,
  variant = "text",
  animation = "wave",
  style,
  ...props
}: SkeletonProps) {
  // The fill colour is the Tailwind class `bg-muted`, NOT an inline style.
  //
  // This object used to carry `backgroundColor: "rgb(var(--color-muted) /
  // <alpha-value>)"`, copied out of tailwind.config.ts:36. `<alpha-value>` is a
  // Tailwind *config* placeholder that the compiler substitutes when it emits a
  // utility class; nothing substitutes it inside a React `style` prop. The
  // browser therefore received `rgb(226 232 240 / <alpha-value>)`, which is not
  // valid CSS, and dropped the declaration — so every skeleton in the app
  // rendered with no fill, leaving only the faint shimmer overlay on a
  // transparent box. Swept 2026-09-06: this was the only `<alpha-value>` outside
  // tailwind.config.ts. Line 34's `/ 0.1` is a real alpha and is left alone.
  const baseStyles = {
    borderRadius: variant === "circular" ? "9999px" : variant === "text" ? "4px" : "0.5rem",
  };

  const variantStyles = {
    text: { height: "1rem", width: "100%" },
    circular: { height: "2.5rem", width: "2.5rem" },
    rectangular: { height: "100%", width: "100%", minHeight: "1rem" },
    card: { height: "100%", width: "100%", minHeight: "12rem", borderRadius: "0.75rem" },
  };

  const waveAnimation = animation === "wave" && (
    <motion.div
      className="absolute inset-0 animate-shimmer"
      style={{
        background: "linear-gradient(90deg, transparent, rgb(var(--color-muted-foreground) / 0.1), transparent)",
        backgroundSize: "200% 100%",
      }}
      animate={{ backgroundPosition: ["-200% 0", "200% 0"] }}
      transition={{ duration: 1.5, repeat: Infinity, ease: "linear" }}
      aria-hidden="true"
    />
  );

  return (
    <div
      className={cn("relative overflow-hidden bg-muted", className)}
      style={{ ...baseStyles, ...variantStyles[variant], ...style }}
      {...props}
    >
      {waveAnimation}
    </div>
  );
}

interface SkeletonTextProps extends React.HTMLAttributes<HTMLDivElement> {
  lines?: number;
}

export function SkeletonText({ className, lines = 3, ...props }: SkeletonTextProps) {
  return (
    <div className={cn("space-y-2", className)} {...props}>
      {Array.from({ length: lines }).map((_, i) => (
        <Skeleton key={i} variant="text" style={{ width: i === lines - 1 ? "70%" : "100%" }} />
      ))}
    </div>
  );
}

// `showTitle` / `showDescription`, not `title` / `description`.
//
// `title?: boolean` is what broke the build: this interface extends
// React.HTMLAttributes<HTMLDivElement>, which declares `title?: string` (the
// real HTML tooltip attribute), so narrowing it to boolean is TS2430 —
// "incorrectly extends… Types of property 'title' are incompatible". Renaming
// is the fix rather than `Omit<…, "title">`, because a caller that wants a
// tooltip on a skeleton card should still be able to pass one and get a string.
// `description` did not conflict (React declares no such attribute) but is
// renamed with it: a props pair where one member is prefixed and the other is
// not is the kind of asymmetry that gets "fixed" back later.
interface SkeletonCardProps extends React.HTMLAttributes<HTMLDivElement> {
  showTitle?: boolean;
  showDescription?: boolean;
  actions?: number;
  contentLines?: number;
}

export function SkeletonCard({
  className,
  showTitle = true,
  showDescription = true,
  actions = 1,
  contentLines = 3,
  ...props
}: SkeletonCardProps) {
  return (
    <div className={cn("rounded-lg border border-border bg-card p-6 space-y-4", className)} {...props}>
      {showTitle && <Skeleton variant="text" style={{ width: "40%", height: "1.25rem" }} />}
      {showDescription && <Skeleton variant="text" style={{ width: "60%", height: "1rem" }} />}
      <SkeletonText lines={contentLines} />
      {actions > 0 && (
        <div className="flex gap-2 pt-2">
          {Array.from({ length: actions }).map((_, i) => (
            <Skeleton key={i} variant="rectangular" style={{ width: "6rem", height: "2.25rem" }} />
          ))}
        </div>
      )}
    </div>
  );
}

interface SkeletonTableProps {
  columns: number;
  rows: number;
  className?: string;
}

export function SkeletonTable({ columns, rows, className }: SkeletonTableProps) {
  return (
    <div className={cn("overflow-x-auto", className)}>
      <table className="w-full" role="table">
        <thead>
          <tr>
            {Array.from({ length: columns }).map((_, i) => (
              <th key={i} className="text-left p-4 font-medium">
                <Skeleton variant="text" style={{ width: "80%" }} />
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {Array.from({ length: rows }).map((_, rowIdx) => (
            <tr key={rowIdx}>
              {Array.from({ length: columns }).map((_, colIdx) => (
                <td key={colIdx} className="p-4">
                  <Skeleton variant="text" style={{ width: "60%" }} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

interface SkeletonListProps {
  items?: number;
  className?: string;
  avatar?: boolean;
  linesPerItem?: number;
}

export function SkeletonList({ items = 5, className, avatar = true, linesPerItem = 2 }: SkeletonListProps) {
  return (
    <div className={cn("space-y-4", className)} role="list">
      {Array.from({ length: items }).map((_, i) => (
        <div key={i} className="flex gap-4 items-start" role="listitem">
          {avatar && <Skeleton variant="circular" />}
          <div className="flex-1 min-w-0 space-y-2">
            <Skeleton variant="text" style={{ width: "30%" }} />
            <SkeletonText lines={linesPerItem} />
          </div>
        </div>
      ))}
    </div>
  );
}

interface SkeletonAvatarProps extends React.HTMLAttributes<HTMLDivElement> {
  size?: "sm" | "md" | "lg" | "xl";
}

export function SkeletonAvatar({ className, size = "md", ...props }: SkeletonAvatarProps) {
  const sizes = {
    sm: "h-8 w-8",
    md: "h-10 w-10",
    lg: "h-12 w-12",
    xl: "h-16 w-16",
  };
  return <Skeleton className={cn(sizes[size], className)} variant="circular" {...props} />;
}