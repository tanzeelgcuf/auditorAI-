"use client";

import { motion, HTMLMotionProps } from "framer-motion";
import { cn } from "../../lib/utils";

/* ── Framer Motion variants ── */

export const fadeIn = {
  hidden: { opacity: 0 },
  visible: { opacity: 1 },
};

export const slideUp = {
  hidden: { opacity: 0, y: 8 },
  visible: { opacity: 1, y: 0 },
};

export const slideDown = {
  hidden: { opacity: 0, y: -8 },
  visible: { opacity: 1, y: 0 },
};

export const slideRight = {
  hidden: { opacity: 0, x: -8 },
  visible: { opacity: 1, x: 0 },
};

export const scaleIn = {
  hidden: { opacity: 0, scale: 0.95 },
  visible: { opacity: 1, scale: 1 },
};

export const staggerContainer = {
  hidden: { opacity: 0 },
  visible: {
    opacity: 1,
    transition: {
      staggerChildren: 0.05,
      delayChildren: 0.1,
    },
  },
};

export const staggerItem = {
  hidden: { opacity: 0, y: 8 },
  visible: { opacity: 1, y: 0 },
};

/* ── Page-level transition ── */
export const pageVariants = {
  initial: { opacity: 0, y: 16 },
  animate: { opacity: 1, y: 0, transition: { duration: 0.3, ease: [0.4, 0, 0.2, 1] } },
  exit: { opacity: 0, y: -8, transition: { duration: 0.2, ease: [0.4, 0, 1, 1] } },
};

/* ── Preset motion components ── */

type VariantKey = "fade" | "slideUp" | "slideDown" | "slideRight" | "scaleIn" | "stagger";

const variants: Record<VariantKey, object> = {
  fade: fadeIn,
  slideUp,
  slideDown,
  slideRight,
  scaleIn,
  stagger: staggerItem,
};

export interface MotionDivProps extends HTMLMotionProps<"div"> {
  variant?: VariantKey;
  className?: string;
}

export function MotionDiv({
  variant = "slideUp",
  className,
  initial = "hidden",
  animate = "visible",
  exit,
  transition,
  ...props
}: MotionDivProps) {
  return (
    <motion.div
      className={cn(className)}
      variants={variants[variant]}
      initial={initial}
      animate={animate}
      exit={exit}
      transition={transition || { duration: 0.2, ease: [0.4, 0, 0.2, 1] }}
      {...props}
    />
  );
}

export interface MotionButtonProps extends HTMLMotionProps<"button"> {
  variant?: VariantKey;
  className?: string;
}

export function MotionButton({
  variant = "scaleIn",
  className,
  whileTap = { scale: 0.97 },
  whileHover = { scale: 1.02 },
  ...props
}: MotionButtonProps) {
  return (
    <motion.button
      className={cn(className)}
      variants={variants[variant]}
      initial="hidden"
      animate="visible"
      whileTap={whileTap}
      whileHover={whileHover}
      transition={{ duration: 0.15, ease: [0.4, 0, 0.2, 1] }}
      {...props}
    />
  );
}

export interface MotionLinkProps extends HTMLMotionProps<"a"> {
  variant?: VariantKey;
  className?: string;
}

export function MotionLink({
  variant = "slideUp",
  className,
  whileTap = { scale: 0.98 },
  ...props
}: MotionLinkProps) {
  return (
    <motion.a
      className={cn(className)}
      variants={variants[variant]}
      initial="hidden"
      animate="visible"
      whileTap={whileTap}
      transition={{ duration: 0.15, ease: [0.4, 0, 0.2, 1] }}
      {...props}
    />
  );
}

/* ── Motion wrapper for lists/grids ── */

export interface StaggerContainerProps extends HTMLMotionProps<"div"> {
  children: React.ReactNode;
  staggerDelay?: number;
  staggerChildren?: number;
}

export function StaggerContainer({
  children,
  staggerDelay = 0.1,
  staggerChildren = 0.05,
  className,
  ...props
}: StaggerContainerProps) {
  return (
    <motion.div
      className={cn(className)}
      variants={staggerContainer}
      initial="hidden"
      animate="visible"
      transition={{ staggerChildren, delayChildren: staggerDelay }}
      {...props}
    >
      {children}
    </motion.div>
  );
}

/* ── Presence-aware exit animations ── */

export interface AnimatePresenceProps {
  children: React.ReactNode;
  initial?: boolean;
}

export function AnimatePresenceWrapper({
  children,
  initial = true,
}: AnimatePresenceProps) {
  // Re-export framer-motion's AnimatePresence with defaults
  return (
    <motion.div
      initial={initial}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={{ duration: 0.2 }}
    >
      {children}
    </motion.div>
  );
}