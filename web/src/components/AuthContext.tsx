"use client";

import React, { createContext, useContext, useEffect, useState } from "react";
import { apiFetch, extractMockPayRef, formatNaira, setDefaultIdentityHeaders } from "@/lib/api";
import { ShieldCheck, CreditCard, AlertTriangle, X, ExternalLink } from "lucide-react";

export interface Persona {
  id: string;
  label: string;
  token: string;
  email: string;
  phone: string;
  displayName: string;
  roleBadge: string;
}

export const PERSONAS: Persona[] = [
  {
    id: "ada",
    label: "Adaeze (Customer)",
    token: "ada",
    email: "ada@meterly.ng",
    phone: "+2348011111111",
    displayName: "Adaeze Okafor",
    roleBadge: "Customer",
  },
  {
    id: "bola",
    label: "Bola (Flatmate)",
    token: "bola",
    email: "bola@meterly.ng",
    phone: "+2348022222222",
    displayName: "Bola Adeyemi",
    roleBadge: "Customer",
  },
  {
    id: "landlord",
    label: "Chief Tunde (Landlord)",
    token: "landlord-tunde",
    email: "tunde@lekkiestates.ng",
    phone: "+2348033333333",
    displayName: "Chief Tunde",
    roleBadge: "Org Owner",
  },
  {
    id: "manager",
    label: "Dayo (Estate Manager)",
    token: "manager-dayo",
    email: "dayo@lekkiestates.ng",
    phone: "+2348044444444",
    displayName: "Dayo Balogun",
    roleBadge: "Org Manager",
  },
  {
    id: "operator",
    label: "Kemi (Operator)",
    token: "operator:kemi",
    email: "kemi.ops@meterly.ng",
    phone: "+2348055555555",
    displayName: "Kemi Ops",
    roleBadge: "Staff Operator",
  },
  {
    id: "admin",
    label: "Segun (Admin)",
    token: "admin:segun",
    email: "segun.admin@meterly.ng",
    phone: "+2348066666666",
    displayName: "Segun Admin",
    roleBadge: "Staff Admin",
  },
];

export interface CheckoutSession {
  checkoutUrl: string;
  totalMinor: number;
  title: string;
  subtitle?: string;
  onComplete?: () => Promise<void> | void;
}

interface AuthContextValue {
  token: string;
  email: string;
  activePersona: Persona | null;
  isPasskeySession: boolean;
  setPersonaById: (id: string) => void;
  setCustomToken: (token: string, email?: string) => void;
  applySession: (token: string, displayName?: string) => void;
  signOut: () => Promise<void>;
  openCheckout: (session: CheckoutSession) => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [token, setToken] = useState<string>(PERSONAS[0].token);
  const [email, setEmail] = useState<string>(PERSONAS[0].email);
  const [checkout, setCheckout] = useState<CheckoutSession | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    try {
      const savedToken = window.localStorage.getItem("meterly_token");
      const savedEmail = window.localStorage.getItem("meterly_email");
      if (savedToken) setToken(savedToken);
      if (savedEmail) setEmail(savedEmail);
    } catch {
      // ignore storage errors
    }
  }, []);

  const setPersonaById = (id: string) => {
    const found = PERSONAS.find((p) => p.id === id);
    if (!found) return;
    setToken(found.token);
    setEmail(found.email);
    try {
      window.localStorage.setItem("meterly_token", found.token);
      window.localStorage.setItem("meterly_email", found.email);
    } catch {
      // ignore
    }
  };

  const setCustomToken = (newTok: string, newEmail?: string) => {
    const clean = newTok.trim();
    setToken(clean);
    if (newEmail) setEmail(newEmail.trim());
    try {
      window.localStorage.setItem("meterly_token", clean);
      if (newEmail) window.localStorage.setItem("meterly_email", newEmail.trim());
    } catch {
      // ignore
    }
  };

  const applySession = (newTok: string, displayName?: string) => {
    const clean = newTok.trim();
    setToken(clean);
    setEmail("");
    try {
      window.localStorage.setItem("meterly_token", clean);
      window.localStorage.removeItem("meterly_email");
      if (displayName) window.localStorage.setItem("meterly_display_name", displayName);
    } catch {
      // ignore
    }
  };

  const signOut = async () => {
    try {
      if (token.startsWith("mls_")) {
        await apiFetch("/v1/auth/logout", { method: "POST", token });
      }
    } catch {
      // ignore
    }
    setPersonaById("ada");
  };

  const activePersona = PERSONAS.find((p) => p.token === token) || null;
  const isPasskeySession = token.startsWith("mls_");

  useEffect(() => {
    const headers: Record<string, string> = {};
    if (email) headers["X-Dev-Verified-Email"] = email;
    if (activePersona?.phone) headers["X-Dev-Verified-Phone"] = activePersona.phone;
    if (activePersona?.displayName) headers["X-Dev-Display-Name"] = activePersona.displayName;
    setDefaultIdentityHeaders(headers);
  }, [token, email, activePersona]);

  const settleMockPayment = async (success: boolean) => {
    if (!checkout) return;
    const ref = extractMockPayRef(checkout.checkoutUrl);
    if (!ref) return;
    setBusy(true);
    setErr(null);
    try {
      await apiFetch(`/v1/dev/payments/${encodeURIComponent(ref)}/complete`, {
        method: "POST",
        body: { success },
      });
      await apiFetch(`/v1/payments/webhooks/mockpay`, {
        method: "POST",
        headers: { "X-Mock-Signature": "dev-signature" },
        body: { reference: ref },
      });
      const cb = checkout.onComplete;
      setCheckout(null);
      if (cb) await cb();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Failed to complete payment");
    } finally {
      setBusy(false);
    }
  };

  const mockRef = checkout ? extractMockPayRef(checkout.checkoutUrl) : null;

  return (
    <AuthContext.Provider
      value={{
        token,
        email,
        activePersona,
        isPasskeySession,
        setPersonaById,
        setCustomToken,
        applySession,
        signOut,
        openCheckout: (s) => {
          setErr(null);
          setCheckout(s);
        },
      }}
    >
      {children}

      {checkout && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-stone-950/60 p-4 backdrop-blur-xs">
          <div className="w-full max-w-md overflow-hidden rounded-2xl border border-stone-200 bg-white shadow-2xl">
            <div className="flex items-center justify-between border-b border-stone-100 bg-[#0B1F1A] px-5 py-4 text-white">
              <div className="flex items-center gap-2.5">
                <ShieldCheck className="h-5 w-5 text-emerald-400" />
                <div>
                  <p className="text-xs font-medium uppercase tracking-wider text-emerald-300/90">
                    CBN-Licensed PSP Checkout
                  </p>
                  <h3 className="text-sm font-semibold">{checkout.title}</h3>
                </div>
              </div>
              <button
                type="button"
                onClick={() => !busy && setCheckout(null)}
                className="rounded-lg p-1 text-stone-300 hover:bg-white/10 hover:text-white"
              >
                <X className="h-4 w-4" />
              </button>
            </div>

            <div className="space-y-4 p-5">
              <div className="rounded-xl bg-stone-50 p-4 border border-stone-200/80">
                <div className="flex items-baseline justify-between">
                  <span className="text-xs font-medium uppercase tracking-wide text-stone-500">
                    Exact Amount Due
                  </span>
                  <span className="font-mono text-2xl font-bold text-stone-900 tabular-nums">
                    {formatNaira(checkout.totalMinor)}
                  </span>
                </div>
                {checkout.subtitle && (
                  <p className="mt-1 text-xs text-stone-600">{checkout.subtitle}</p>
                )}
              </div>

              <p className="text-xs text-stone-600 leading-relaxed">
                Meterly never holds stored-value balances or vends electricity before server-side payment verification confirms the exact quote total.
              </p>

              {err && (
                <div className="rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-800">
                  {err}
                </div>
              )}

              {mockRef ? (
                <div className="space-y-2.5 pt-1">
                  <p className="text-[11px] font-medium uppercase tracking-wider text-stone-500">
                    Sandbox Payment Simulator ({mockRef})
                  </p>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => settleMockPayment(true)}
                    className="flex w-full items-center justify-center gap-2 rounded-xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white shadow-xs transition hover:bg-emerald-700 disabled:opacity-50"
                  >
                    <CreditCard className="h-4 w-4" />
                    {busy
                      ? "Verifying & Vending..."
                      : `Pay ${formatNaira(checkout.totalMinor)} (Card •••• 4081)`}
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => settleMockPayment(false)}
                    className="flex w-full items-center justify-center gap-2 rounded-xl border border-stone-300 bg-white px-4 py-2.5 text-xs font-medium text-stone-700 transition hover:bg-stone-50 disabled:opacity-50"
                  >
                    <AlertTriangle className="h-3.5 w-3.5 text-amber-600" />
                    Simulate Declined / Abandoned Checkout
                  </button>
                </div>
              ) : (
                <a
                  href={checkout.checkoutUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="flex w-full items-center justify-center gap-2 rounded-xl bg-emerald-600 px-4 py-3 text-sm font-semibold text-white shadow-xs transition hover:bg-emerald-700"
                >
                  Continue to Paystack Checkout
                  <ExternalLink className="h-4 w-4" />
                </a>
              )}
            </div>
          </div>
        </div>
      )}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used inside AuthProvider");
  }
  return ctx;
}
