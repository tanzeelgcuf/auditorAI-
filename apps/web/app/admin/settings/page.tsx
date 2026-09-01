"use client";

import { useEffect, useState } from "react";
import { Shell } from "../../../components/layout/shell";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "../../../components/ui/card";
import { Input } from "../../../components/ui/input";
import { useFirmSettings, useUpdateFirmSettings } from "../../../lib/hooks";
import { Palette, CreditCard, Loader2, Check } from "lucide-react";
import { MotionDiv, MotionButton } from "../../../components/ui/motion";

export default function SettingsPage() {
  const { data } = useFirmSettings();
  const update = useUpdateFirmSettings();

  const [color, setColor] = useState("#0F172A");
  const [footer, setFooter] = useState("");

  useEffect(() => {
    if (data) {
      setColor(data.brand_primary_color || "#0F172A");
      setFooter(data.report_footer_text || "");
    }
  }, [data]);

  const [saved, setSaved] = useState(false);

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    await update.mutateAsync({ brand_primary_color: color, report_footer_text: footer });
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  }

  return (
    <Shell>
      <MotionDiv variant="slideUp" className="mb-6">
        <h1 className="text-2xl font-semibold text-foreground flex items-center gap-2">
          <Palette className="h-6 w-6" aria-hidden="true" />
          Firm Settings
        </h1>
      </MotionDiv>

      <div className="max-w-lg space-y-6">
        <MotionDiv variant="slideUp">
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Palette className="h-5 w-5" aria-hidden="true" />
                Branding
              </CardTitle>
              <CardDescription>
                Applied to generated audit report PDFs. The methodology disclosure always stays
                regardless of branding.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <form onSubmit={handleSave} className="space-y-4">
                <div className="flex items-center gap-3">
                  <label className="w-32 text-sm font-medium text-foreground">Primary color</label>
                  <input
                    type="color"
                    value={color}
                    onChange={(e) => setColor(e.target.value)}
                    className="h-9 w-14 cursor-pointer rounded border border-input bg-transparent"
                  />
                  <span className="font-mono text-xs text-muted-foreground">{color}</span>
                </div>
                <div className="space-y-1">
                  <label className="text-sm font-medium text-foreground">Report footer text</label>
                  <Input
                    value={footer}
                    onChange={(e) => setFooter(e.target.value)}
                    placeholder="Your firm's disclaimer / contact line"
                  />
                </div>
                <MotionButton type="submit" disabled={update.isPending} className="gap-2">
                  {update.isPending ? (
                    <>
                      <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
                      Saving…
                    </>
                  ) : (
                    <>
                      <Check className="h-4 w-4" aria-hidden="true" />
                      Save
                    </>
                  )}
                </MotionButton>
                {saved && (
                  <span className="ml-3 text-sm text-success flex items-center gap-1">
                    <Check className="h-4 w-4" aria-hidden="true" />
                    Saved
                  </span>
                )}
              </form>
            </CardContent>
          </Card>
        </MotionDiv>

        <MotionDiv variant="slideUp" style={{ transitionDelay: "100ms" }}>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <CreditCard className="h-5 w-5" aria-hidden="true" />
                Billing
              </CardTitle>
              <CardDescription>Managed via Stripe Checkout.</CardDescription>
            </CardHeader>
            <CardContent>
              <a href={`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/v1/billing/checkout`}>
                <MotionButton variant="secondary" className="gap-2">
                  <CreditCard className="h-4 w-4" aria-hidden="true" />
                  Manage subscription
                </MotionButton>
              </a>
            </CardContent>
          </Card>
        </MotionDiv>
      </div>
    </Shell>
  );
}