"use client";

import type { ComponentProps } from "react";
import { ThemeProvider as NextThemesProvider } from "next-themes";

// The props type is DERIVED from the component, not imported by name.
//
// This file used to do `import { type ThemeProviderProps } from
// "next-themes/dist/types"`, which is the exact error in shadcn-ui/ui#5706:
// next-themes deleted that subpath (see pacocoursey/next-themes#322,
// "Deletion of the ThemeProviderProps"), so on the pinned ^0.4.6 the module
// does not resolve and tsc fails on the import alone.
//
// The obvious repair is `import type { ThemeProviderProps } from "next-themes"`.
// `ComponentProps<typeof NextThemesProvider>` is used instead because it is
// correct by construction: it needs no named type export to exist, and it
// cannot drift again the next time upstream reorganises its type entry points.
// That matters here specifically — node_modules is absent in this environment,
// so which named types 0.4.6 re-exports from its root could not be checked, and
// this form does not depend on the answer.
type ThemeProviderProps = ComponentProps<typeof NextThemesProvider>;

export function ThemeProvider({ children, ...props }: ThemeProviderProps) {
  return <NextThemesProvider {...props}>{children}</NextThemesProvider>;
}