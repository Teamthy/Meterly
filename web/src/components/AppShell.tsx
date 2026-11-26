"use client";

import React, { useEffect, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { PERSONAS, useAuth } from "./AuthContext";
import {
  Zap,
  Share2,
  Users,
  Home,
  CalendarClock,
  Building2,
  Smartphone,
  ShieldAlert,
  UserCheck,
  MessageCircle,
  Headphones,
  UserRound,
  Settings,
  Fingerprint,
  FlaskConical,
} from "lucide-react";

const NAV_ITEMS = [
  { href: "/", label: "On the meter", icon: Zap },
  { href: "/onboarding", label: "Setup", icon: UserCheck },
  { href: "/profile", label: "Profile", icon: UserRound },
  { href: "/settings", label: "Settings", icon: Settings },
  { href: "/signin", label: "Passkey", icon: Fingerprint },
  { href: "/households", label: "Household", icon: Home },
  { href: "/pay-for-me", label: "Pay-for-Me", icon: Share2 },
  { href: "/pots", label: "Split the Meter", icon: Users },
  { href: "/autopower", label: "AutoPower", icon: CalendarClock },
  { href: "/power-desk", label: "Power Desk", icon: Building2 },
  { href: "/offline", label: "Offline *347#", icon: Smartphone },
  { href: "/support", label: "Support", icon: MessageCircle },
  { href: "/admin/support", label: "Support Inbox", icon: Headphones },
  { href: "/admin", label: "Ops Console", icon: ShieldAlert },
  { href: "/dev", label: "Sandbox", icon: FlaskConical },
];

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const { token, activePersona, setPersonaById, setCustomToken } = useAuth();
  const isStaff = token.startsWith("operator:") || token.startsWith("admin:");
  const navItems = NAV_ITEMS.filter(
    (item) => item.href !== "/admin/support" || isStaff
  );
  const [ready, setReady] = useState<boolean | null>(null);
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

  const isPublicGuestPage =
    pathname?.startsWith("/p/") ||
    (pathname?.startsWith("/pots/") && pathname !== "/pots");

  return (
    <div className="min-h-screen flex flex-col bg-[#F7F8F5] text-stone-900">
      <header className="sticky top-0 z-30 border-b border-stone-200/90 bg-[#0B1F1A] text-white">
        <div className="mx-auto flex max-w-7xl flex-wrap items-center justify-between gap-3 px-4 py-3 sm:px-6">
          <div className="flex items-center gap-4">
            <Link href="/" className="flex items-center gap-2.5 group">
              <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-emerald-500 text-[#0B1F1A] shadow-xs transition group-hover:bg-emerald-400">
                <Zap className="h-5 w-5 fill-current" />
              </div>
              <div>
                <div className="flex items-center gap-2">
                  <span className="text-base font-bold tracking-tight">Meterly</span>
                  <span className="rounded-md bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-emerald-300 border border-emerald-400/20">
                    NG
                  </span>
                </div>
                <p className="text-[11px] text-stone-300/80 hidden sm:block">
                  Credit on the meter. Token only if the meter can&apos;t take it.
                </p>
              </div>
            </Link>
          </div>

          <div className="flex items-center gap-2.5">
            <div className="hidden md:flex items-center gap-1.5 rounded-full bg-white/5 px-2.5 py-1 text-xs text-stone-300 border border-white/10">
              <span
                className={`h-2 w-2 rounded-full ${
                  ready === true
                    ? "bg-emerald-400"
                    : ready === false
                    ? "bg-rose-400"
                    : "bg-amber-400"
                }`}
              />
              <span>{ready === true ? "Engine Ready" : ready === false ? "API Offline" : "Checking"}</span>
            </div>

            {!isPublicGuestPage && (
              <div className="flex items-center gap-1.5 rounded-xl bg-white/10 px-2.5 py-1.5 border border-white/10">
                <UserCheck className="h-3.5 w-3.5 text-emerald-300 shrink-0" />
                <select
                  aria-label="Active Persona"
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
                  className="bg-transparent text-xs font-medium text-white focus:outline-none cursor-pointer"
                >
                  {PERSONAS.map((p) => (
                    <option key={p.id} value={p.id} className="text-stone-900">
                      {p.label}
                    </option>
                  ))}
                  <option value="__custom__" className="text-stone-900">
                    Custom Bearer / OIDC JWT…
                  </option>
                </select>
              </div>
            )}
          </div>
        </div>

        {showCustomAuth && !isPublicGuestPage && (
          <div className="border-t border-white/10 bg-[#0F2A23] px-4 py-2.5">
            <div className="mx-auto flex max-w-7xl flex-wrap items-center gap-2">
              <input
                type="text"
                value={draftTok}
                onChange={(e) => setDraftTok(e.target.value)}
                placeholder="Enter dev subject (e.g. customer-1, operator:ops1) or RS256 OIDC JWT"
                className="flex-1 rounded-lg bg-black/30 px-3 py-1.5 font-mono text-xs text-white placeholder-stone-400 border border-white/15 focus:outline-none focus:border-emerald-400"
              />
              <button
                type="button"
                onClick={() => {
                  if (draftTok.trim()) setCustomToken(draftTok.trim());
                  setShowCustomAuth(false);
                }}
                className="rounded-lg bg-emerald-500 px-3 py-1.5 text-xs font-semibold text-[#0B1F1A] hover:bg-emerald-400"
              >
                Apply Token
              </button>
            </div>
          </div>
        )}

        <nav className="border-t border-white/10 bg-[#0E2620]">
          <div className="mx-auto flex max-w-7xl items-center gap-1 overflow-x-auto px-4 py-1.5 sm:px-6">
            {navItems.map((item) => {
              const Icon = item.icon;
              const active =
                item.href === "/"
                  ? pathname === "/"
                  : pathname === item.href || pathname?.startsWith(item.href + "/");
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  className={`flex shrink-0 items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium transition ${
                    active
                      ? "bg-emerald-500 text-[#0B1F1A] font-semibold shadow-xs"
                      : "text-stone-300 hover:bg-white/10 hover:text-white"
                  }`}
                >
                  <Icon className="h-3.5 w-3.5" />
                  {item.label}
                </Link>
              );
            })}
          </div>
        </nav>
      </header>

      <main className="mx-auto w-full max-w-7xl flex-1 px-4 py-6 sm:px-6">
        {children}
      </main>

      <footer className="border-t border-stone-200 bg-white py-4 text-xs text-stone-500">
        <div className="mx-auto flex max-w-7xl flex-wrap items-center justify-between gap-2 px-4 sm:px-6">
          <p>
            Selling point: units on the meter. We never call remote credit delivered until the meter confirms.
          </p>
          <p className="font-mono text-[11px] text-stone-400">
            STS token is fallback for keypad meters • Payments via CBN-licensed PSP
          </p>
        </div>
      </footer>
    </div>
  );
}
