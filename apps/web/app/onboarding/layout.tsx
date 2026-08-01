import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Set up AI Auditor",
};

export default function OnboardingLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-50 p-4">
      <div className="w-full max-w-lg">
        <div className="mb-6 text-center">
          <div className="text-xl font-semibold text-slate-900">AI Auditor</div>
          <div className="text-sm text-slate-500">Set up your firm in 3 steps</div>
        </div>
        {children}
      </div>
    </div>
  );
}
