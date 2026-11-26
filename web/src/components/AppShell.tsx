"use client";

import React, { useEffect, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { PERSONAS, useAuth } from "./AuthContext";
import {
  Share2,
  Users,
  Building2,
  Smartphone,
  ShieldAlert,
  UserCheck,
  Headphones,
  UserRound,
  Settings,
  Fingerprint,
  FlaskConical,
  ChevronDown,
  Shield,
} from "lucide-react";

const PRIMARY = [
  { href: "/", label: "Power" },
  { href: "/autopower", label: "Auto", soon: true },
  { href: "/households", label: "House" },
  { href: "/support", label: "Help" },
];

const MORE = [
  { href: "/pay-for-me", label: "Pay-for-Me", icon: Share2 },
  { href: "/pots", label: "Split the meter", icon: Users },
  { href: "/power-desk", label: "Power Desk", icon: Building2 },
  { href: "/offline", label: "USSD *347#", icon: Smartphone },
  { href: "/onboarding", label: "Setup", icon: UserCheck },
  { href: "/profile", label: "Profile", icon: UserRound },
  { href: "/settings", label: "Settings", icon: Settings },
  { href: "/security", label: "Security", icon: Shield },
  { href: "/signin", label: "Passkey", icon: Fingerprint },
];

const STAFF = [
  { href: "/admin", label: "Ops", icon: ShieldAlert },
  { href: "/admin/support", label: "Inbox", icon: Headphones },
  { href: "/dev", label: "Sandbox", icon: FlaskConical },
];

function BrandMark() {
  return (
    <svg viewBox="0 0 32 32" className="h-8 w-8" aria-hidden>
      <rect width="32" height="32" rx="9" fill="#D8FF3E" />
      <path
        d="M9 23V9.5L16 17.2 23 9.5V23"
        fill="none"
        stroke="#10140A"
        strokeWidth="2.3"
        strokeLinejoin="round"
      />
      <path d="M9 23h14" stroke="#10140A" strokeWidth="2.3" />
    </svg>
  );
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const { token, activePersona, setPersonaById, setCustomToken } = useAuth();
  const isStaff = token.startsWith("operator:") || token.startsWith("admin:");
  const [ready, setReady] = useState<boolean | null>(null);
  const [moreOpen, setMoreOpen] = useState(false);
  const [showCustomAuth, setShowCustomAuth] = useState(false);
  const [draftTok, setDraftTok] = useState("");

  useEffect(() => {
    let cancelled = false;
    fetch("/readyz", { cache: "no-store" })
      .then((r) => {
        if (!cancelled) setReady(r.ok);
      })
      .catch(() => {
        if (!cancelled) setReady(false);
      });
    return () => {
      cancelled = true;
    };
  }, [pathname]);

  useEffect(() => {
    setMoreOpen(false);
  }, [pathname]);

  const isPublicGuestPage =
    pathname?.startsWith("/p/") ||
    (pathname?.startsWith("/pots/") && pathname !== "/pots");

  const moreItems = isStaff ? [...MORE, ...STAFF] : [...MORE, STAFF[2]];
  const moreActive = moreItems.some(
    (item) => pathname === item.href || pathname?.startsWith(item.href + "/")
  );

  return (
    <div className="ml-shell flex min-h-dvh flex-col">
      <header className="sticky top-0 z-30 border-b border-white/10 bg-ink/80 backdrop-blur-xl">
        <div className="mx-auto flex max-w-6xl items-center justify-between gap-3 px-4 py-3 sm:px-6">
          <Link href="/" className="flex items-center gap-3">
            <BrandMark />
            <div>
              <div className="flex items-baseline gap-2">
                <span className="font-display text-lg font-semibold tracking-tight text-paper">
                  Meterly
                </span>
                <span className="text-[10px] font-semibold uppercase tracking-[0.18em] text-white/40">
                  NG
                </span>
              </div>
              <p className="hidden text-[11px] text-white/45 sm:block">
                Credit on the meter.
              </p>
            </div>
          </Link>

          <nav className="hidden items-center gap-1 md:flex">
            {PRIMARY.map((item) => {
              const active =
                item.href === "/"
                  ? pathname === "/"
                  : pathname === item.href || pathname?.startsWith(item.href + "/");
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  className={`relative rounded-full px-3.5 py-1.5 text-[13px] font-medium transition ${
                    active
                      ? "bg-volt text-volt-ink"
                      : "text-white/70 hover:bg-white/10 hover:text-paper"
                  }`}
                >
                  {item.label}
                  {item.soon && !active ? (
                    <span className="ml-1.5 text-[9px] font-semibold uppercase tracking-wider text-volt/80">
                      soon
                    </span>
                  ) : null}
                </Link>
              );
            })}
            <div className="relative">
              <button
                type="button"
                onClick={() => setMoreOpen((v) => !v)}
                className={`inline-flex items-center gap-1 rounded-full px-3.5 py-1.5 text-[13px] font-medium ${
                  moreActive
                    ? "bg-white/12 text-paper"
                    : "text-white/70 hover:bg-white/10 hover:text-paper"
                }`}
              >
                More
                <ChevronDown className="h-3.5 w-3.5" />
              </button>
              {moreOpen && (
                <div className="absolute right-0 mt-2 w-52 overflow-hidden rounded-2xl border border-white/10 bg-[#12141a] py-1 shadow-2xl">
                  {moreItems.map((item) => {
                    const Icon = item.icon;
                    const active =
                      pathname === item.href || pathname?.startsWith(item.href + "/");
                    return (
                      <Link
                        key={item.href}
                        href={item.href}
                        className={`flex items-center gap-2 px-3.5 py-2 text-[13px] ${
                          active
                            ? "bg-volt/15 text-volt"
                            : "text-white/75 hover:bg-white/6 hover:text-paper"
                        }`}
                      >
                        <Icon className="h-3.5 w-3.5" />
                        {item.label}
                      </Link>
                    );
                  })}
                </div>
              )}
            </div>
          </nav>

          <div className="flex items-center gap-2">
            <span
              className={`hidden h-1.5 w-1.5 rounded-full sm:inline-block ${
                ready === true
                  ? "bg-volt"
                  : ready === false
                    ? "bg-rose-400"
                    : "bg-amber-400"
              }`}
              title={ready === true ? "API ready" : ready === false ? "API offline" : "Checking"}
            />
            {!isPublicGuestPage && (
              <select
                aria-label="Active persona"
                value={activePersona?.id || "__custom__"}
                onChange={(e) => {
                  if (e.target.value === "__custom__") {
                    setDraftTok(token);
                    setShowCustomAuth(true);
                  } else {
                    setShowCustomAuth(false);
                    setPersonaById(e.target.value);
                  }
                }}
                className="max-w-[9.5rem] truncate rounded-full border border-white/10 bg-white/6 px-3 py-1.5 text-[12px] text-paper focus:outline-none"
              >
                {PERSONAS.map((p) => (
                  <option key={p.id} value={p.id} className="text-stone-900">
                    {p.displayName}
                  </option>
                ))}
                <option value="__custom__" className="text-stone-900">
                  Custom token…
                </option>
              </select>
            )}
          </div>
        </div>

        <nav className="flex gap-1 overflow-x-auto border-t border-white/10 px-3 py-2 md:hidden">
          {PRIMARY.map((item) => {
            const active =
              item.href === "/"
                ? pathname === "/"
                : pathname === item.href || pathname?.startsWith(item.href + "/");
            return (
              <Link
                key={item.href}
                href={item.href}
                className={`shrink-0 rounded-full px-3 py-1.5 text-xs font-medium ${
                  active ? "bg-volt text-volt-ink" : "text-white/70"
                }`}
              >
                {item.label}
              </Link>
            );
          })}
        </nav>

        {showCustomAuth && !isPublicGuestPage && (
          <div className="border-t border-white/10 px-4 py-2.5">
            <div className="mx-auto flex max-w-6xl gap-2">
              <input
                type="text"
                value={draftTok}
                onChange={(e) => setDraftTok(e.target.value)}
                placeholder="Bearer subject or OIDC JWT"
                className="flex-1 rounded-lg border border-white/10 bg-black/40 px-3 py-1.5 font-mono text-xs text-paper placeholder-white/30 focus:outline-none"
              />
              <button
                type="button"
                onClick={() => {
                  if (draftTok.trim()) setCustomToken(draftTok.trim());
                  setShowCustomAuth(false);
                }}
                className="rounded-lg bg-volt px-3 py-1.5 text-xs font-semibold text-volt-ink"
              >
                Apply
              </button>
            </div>
          </div>
        )}
      </header>

      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 text-paper sm:px-6 sm:py-8">
        {children}
      </main>

      <footer className="border-t border-white/10 py-4 text-[11px] text-white/40">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-2 px-4 sm:px-6">
          <p>Units on the meter. Delivered only when the meter confirms.</p>
          <p className="font-mono">STS is fallback · CBN-licensed PSP</p>
        </div>
      </footer>
    </div>
  );
}
