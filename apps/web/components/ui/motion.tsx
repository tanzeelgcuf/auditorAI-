"use client";

import { motion, type HTMLMotionProps, type Variants } from "framer-motion";
import { type VariantProps } from "class-variance-authority";
import { buttonVariants } from "./button";
import { cn } from "../../lib/utils";

/* ── Framer Motion variants ──
 *
 * Every object below is annotated `Variants` on purpose. Without the
 * annotation TypeScript widens `ease: [0.4, 0, 0.2, 1]` to `number[]`, which is
 * not assignable to framer-motion's `Easing` (a cubic bezier is the 4-tuple
 * `[number, number, number, number]`). The annotation supplies the contextual
 * type that keeps the literal a tuple. That is why `pageVariants` — the only
 * object here that carries a `transition` — is the one that failed to compile.
 */

export const fadeIn: Variants = {
  hidden: { opacity: 0 },
  visible: { opacity: 1 },
};

export const slideUp: Variants = {
  hidden: { opacity: 0, y: 8 },
  visible: { opacity: 1, y: 0 },
};

export const slideDown: Variants = {
  hidden: { opacity: 0, y: -8 },
  visible: { opacity: 1, y: 0 },
};

export const slideRight: Variants = {
  hidden: { opacity: 0, x: -8 },
  visible: { opacity: 1, x: 0 },
};

export const scaleIn: Variants = {
  hidden: { opacity: 0, scale: 0.95 },
  visible: { opacity: 1, scale: 1 },
};

export const staggerContainer: Variants = {
  hidden: { opacity: 0 },
  visible: {
    opacity: 1,
    transition: {
      staggerChildren: 0.05,
      delayChildren: 0.1,
    },
  },
};

export const staggerItem: Variants = {
  hidden: { opacity: 0, y: 8 },
  visible: { opacity: 1, y: 0 },
};

/* ── Page-level transition ── */
export const pageVariants: Variants = {
  initial: { opacity: 0, y: 16 },
  animate: { opacity: 1, y: 0, transition: { duration: 0.3, ease: [0.4, 0, 0.2, 1] } },
  exit: { opacity: 0, y: -8, transition: { duration: 0.2, ease: [0.4, 0, 1, 1] } },
};

/* ── Preset motion components ── */

// KEY RENAMED `fade` -> `fadeIn` (2026-09-06). It is named after the exported
// object it points at, and the old mismatch was a live bug, not a style
// preference: pdf-viewer.tsx:87 passes `variant="fadeIn"`, and grep found the
// literal `"fade"` in exactly one place in the whole app — this type
// declaration. No call site ever used the key that existed. `presets["fadeIn"]`
// was therefore `undefined`, and an `undefined` variants prop next to
// `initial="hidden" animate="visible"` does not throw; the animation just
// silently never runs. Renaming the key fixes the call site and removes the
// trap, where "fix the call site to `fade`" would have left it.
export type VariantKey = "fadeIn" | "slideUp" | "slideDown" | "slideRight" | "scaleIn" | "stagger";

// Named `presets`, not `variants`. The old name shadowed framer-motion's own
// `variants` prop inside these components, which is how `className={cn(className)}`
// managed to read as complete for as long as it did.
const presets: Record<VariantKey, Variants> = {
  fadeIn,
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
      variants={presets[variant]}
      initial={initial}
      animate={animate}
      exit={exit}
      transition={transition || { duration: 0.2, ease: [0.4, 0, 0.2, 1] }}
      {...props}
    />
  );
}

/* ── MotionButton ──
 *
 * REWRITTEN 2026-09-06. `variant` here is the shadcn Button style variant, NOT
 * a motion preset; the animation preset moved to `motionVariant`.
 *
 * The CI failure was reported as a type error — `variant="secondary"` /
 * `size="icon"` are not assignable to the old `variant?: VariantKey`. The
 * proposed fix was to widen `VariantKey` with "secondary" | "outline" | "ghost".
 * That would have compiled and been wrong twice over: `presets["ghost"]` is
 * `undefined`, and the button would still have had no button styling.
 *
 * OBSERVED, by reading all 19 call sites: this component rendered
 * `className={cn(className)}` and never composed `buttonVariants`. The 19 sites
 * pass only layout classes (`w-full gap-2`, `gap-2`, `h-8 w-8`) — not one of
 * them re-declares a background, height, padding, radius or focus ring. So
 * every MotionButton in the app rendered as an unstyled <button>: the submit
 * CTAs on login:106, signup:125, dashboard:70, onboarding:96 and
 * settings:76 among them, and their `disabled={…isPending}` states got no
 * `disabled:opacity-50` either. Only the 9 sites that passed `variant`/`size`
 * were visible to tsc; the other 10 typechecked clean and were equally
 * unstyled. The type error was the symptom; the missing recipe was the bug.
 *
 * `variant` keeps its name because every real call site already uses it the way
 * shadcn does, and because a <button> having a *style* variant is the less
 * surprising reading. MotionDiv/MotionLink keep `variant` as the motion preset
 * — a div has no style variants, and 43 `variant="slideUp"` sites should not be
 * churned to prove a point about symmetry.
 */
export type MotionButtonProps = Omit<HTMLMotionProps<"button">, "variants"> &
  VariantProps<typeof buttonVariants> & {
    motionVariant?: VariantKey;
    className?: string;
  };

export function MotionButton({
  variant,
  size,
  motionVariant = "scaleIn",
  className,
  whileTap = { scale: 0.97 },
  whileHover = { scale: 1.02 },
  ...props
}: MotionButtonProps) {
  return (
    <motion.button
      className={cn(buttonVariants({ variant, size }), className)}
      variants={presets[motionVariant]}
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
      variants={presets[variant]}
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