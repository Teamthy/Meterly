"use client";

import React, { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/components/AuthContext";
import { apiFetch, DISCOS, newIdempotencyKey } from "@/lib/api";
import { OnboardingStep, UserProfile } from "@/lib/types";
import { createPasskey, CreationOptions } from "@/lib/webauthn";
import { Bell, Fingerprint, Home, User, Zap } from "lucide-react";

const ORDER: OnboardingStep[] = ["welcome", "account", "profile", "meter", "alerts", "done"];

const COPY: Record<Exclude<OnboardingStep, "done">, { title: string; body: string }> = {
  welcome: {
    title: "Credit lands on the meter",
    body: "On compatible smart meters Meterly pushes units remotely. Keypad meters still get a licensed STS token. We only call it delivered when the meter confirms.",
  },
  account: {
    title: "Unlock this device",
    body: "Register a passkey so this phone’s biometrics sign you in. No password stored on Meterly. You can skip and keep your current session.",
  },
  profile: {
    title: "Your name and contacts",
    body: "Saved encrypted on your user row. Receipts and token SMS use these — we never log the raw values.",
  },
  meter: {
    title: "Add the first meter",
    body: "Validate with the DisCo, then save it. Skip if you’ll do this later from the home screen.",
  },
  alerts: {
    title: "How we reach you",
    body: "Transactional SMS and email are on by default. WhatsApp stays opt-in.",
  },
};

function nextStep(step: OnboardingStep): OnboardingStep {
  const i = ORDER.indexOf(step);
  return ORDER[Math.min(i + 1, ORDER.length - 1)];
}

export default function OnboardingPage() {
  const { token, applySession } = useAuth();
  const router = useRouter();
  const [step, setStep] = useState<OnboardingStep>("welcome");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [phone, setPhone] = useState("");
  const [meter, setMeter] = useState("70011112222");
  const [disco, setDisco] = useState("IKEDC");
  const [sms, setSms] = useState(true);
  const [emailOn, setEmailOn] = useState(true);
  const [whatsapp, setWhatsapp] = useState(false);

  useEffect(() => {
    let cancelled = false;
    apiFetch<{ step: OnboardingStep; profile?: UserProfile }>("/v1/me/onboarding", { token })
      .then((r) => {
        if (!cancelled && r.step) setStep(r.step);
      })
      .catch(() => {
        apiFetch<{ profile: UserProfile }>("/v1/me", { token })
          .then((r) => {
            if (!cancelled) {
              setStep(r.profile.onboarding_step || "welcome");
              setName(r.profile.display_name || "");
            }
          })
          .catch(() => undefined);
      });
    return () => {
      cancelled = true;
    };
  }, [token]);

  const advance = async (to: OnboardingStep, sessionToken = token) => {
    const r = await apiFetch<{ step: OnboardingStep }>("/v1/me/onboarding/advance", {
      method: "POST",
      token: sessionToken,
      body: { step: to },
    });
    setStep(r.step);
    if (r.step === "done") router.push("/");
  };

  const onContinue = async () => {
    setBusy(true);
    setErr(null);
    try {
      if (step === "profile") {
        await apiFetch("/v1/me", {
          method: "PATCH",
          token,
          body: {
            display_name: name,
            email: email || undefined,
            phone: phone || undefined,
          },
        });
      }
      if (step === "meter" && meter.trim()) {
        await apiFetch("/v1/meters", {
          method: "POST",
          token,
          idempotencyKey: newIdempotencyKey("onboard-meter"),
          body: { meter_number: meter.trim(), disco_code: disco, label: "Home" },
        });
      }
      if (step === "alerts") {
        await apiFetch("/v1/me/settings", {
          method: "PATCH",
          token,
          body: { sms, email_enabled: emailOn, whatsapp },
        });
      }
      await advance(nextStep(step));
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Could not save this step");
    } finally {
      setBusy(false);
    }
  };

  const onPasskey = async () => {
    setBusy(true);
    setErr(null);
    try {
      const origin = window.location.origin;
      const opt = await apiFetch<CreationOptions>("/v1/auth/passkeys/register/begin", {
        method: "POST",
        token,
        body: { display_name: name || undefined, origin },
      });
      const cred = await createPasskey(opt);
      const done = await apiFetch<{ token: string }>("/v1/auth/passkeys/register/finish", {
        method: "POST",
        body: { origin, name: "This device", ...cred },
      });
      applySession(done.token, name);
      await advance("profile", done.token);
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Passkey registration failed");
    } finally {
      setBusy(false);
    }
  };

  if (step === "done") {
    return (
      <div className="mx-auto max-w-lg rounded-2xl border border-stone-200 bg-white p-8 text-center">
        <Zap className="mx-auto h-10 w-10 text-emerald-600" />
        <h1 className="mt-4 text-xl font-semibold">You’re set</h1>
        <p className="mt-2 text-sm text-stone-600">Buy units whenever the house needs them.</p>
        <button
          type="button"
          onClick={() => router.push("/")}
          className="mt-6 rounded-xl bg-emerald-600 px-4 py-2.5 text-sm font-semibold text-white"
        >
          Go to the meter
        </button>
      </div>
    );
  }

  const meta = COPY[step];
  const idx = ORDER.indexOf(step);

  return (
    <div className="mx-auto max-w-lg">
      <p className="text-xs font-semibold uppercase tracking-wider text-emerald-700">
        Setup {idx + 1} of 5
      </p>
      <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-stone-200">
        <div className="h-full bg-emerald-500 transition-all" style={{ width: `${((idx + 1) / 5) * 100}%` }} />
      </div>
      <h1 className="mt-6 text-2xl font-semibold tracking-tight">{meta.title}</h1>
      <p className="mt-2 text-sm leading-relaxed text-stone-600">{meta.body}</p>

      {step === "account" && (
        <button
          type="button"
          disabled={busy}
          onClick={onPasskey}
          className="mt-6 flex w-full items-center justify-center gap-2 rounded-xl bg-[#0B1F1A] px-4 py-3 text-sm font-semibold text-white hover:bg-[#16352c] disabled:opacity-50"
        >
          <Fingerprint className="h-4 w-4" />
          Create a device passkey
        </button>
      )}

      {step === "profile" && (
        <div className="mt-6 space-y-3">
          <label className="block text-xs font-medium text-stone-600">
            Display name
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="mt-1 w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm"
            />
          </label>
          <label className="block text-xs font-medium text-stone-600">
            Email
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="mt-1 w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm"
            />
          </label>
          <label className="block text-xs font-medium text-stone-600">
            Phone (E.164)
            <input
              value={phone}
              onChange={(e) => setPhone(e.target.value)}
              placeholder="+23480…"
              className="mt-1 w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm"
            />
          </label>
        </div>
      )}

      {step === "meter" && (
        <div className="mt-6 space-y-3">
          <label className="block text-xs font-medium text-stone-600">
            Meter number
            <input
              value={meter}
              onChange={(e) => setMeter(e.target.value)}
              className="mt-1 w-full rounded-xl border border-stone-300 bg-white px-3 py-2 font-mono text-sm"
            />
          </label>
          <label className="block text-xs font-medium text-stone-600">
            DisCo
            <select
              value={disco}
              onChange={(e) => setDisco(e.target.value)}
              className="mt-1 w-full rounded-xl border border-stone-300 bg-white px-3 py-2 text-sm"
            >
              {DISCOS.map((d) => (
                <option key={d.code} value={d.code}>
                  {d.name}
                </option>
              ))}
            </select>
          </label>
        </div>
      )}

      {step === "alerts" && (
        <div className="mt-6 space-y-2">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={sms} onChange={(e) => setSms(e.target.checked)} />
            SMS receipts
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={emailOn} onChange={(e) => setEmailOn(e.target.checked)} />
            Email receipts
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={whatsapp} onChange={(e) => setWhatsapp(e.target.checked)} />
            WhatsApp (opt-in)
          </label>
        </div>
      )}

      {err && <p className="mt-4 text-sm text-rose-700">{err}</p>}

      <div className="mt-8 flex gap-3">
        {step === "meter" && (
          <button
            type="button"
            disabled={busy}
            onClick={async () => {
              setMeter("");
              setBusy(true);
              try {
                await advance("alerts");
              } catch (e) {
                setErr(e instanceof Error ? e.message : "Could not skip");
              } finally {
                setBusy(false);
              }
            }}
            className="rounded-xl border border-stone-300 px-4 py-2.5 text-sm font-medium text-stone-700"
          >
            Skip meter
          </button>
        )}
        {step === "account" && (
          <button
            type="button"
            disabled={busy}
            onClick={onContinue}
            className="rounded-xl border border-stone-300 px-4 py-2.5 text-sm font-medium text-stone-700"
          >
            Continue without passkey
          </button>
        )}
        {step !== "account" && (
          <button
            type="button"
            disabled={busy}
            onClick={onContinue}
            className="ml-auto rounded-xl bg-emerald-600 px-4 py-2.5 text-sm font-semibold text-white hover:bg-emerald-700 disabled:opacity-50"
          >
            {busy ? "Saving…" : step === "alerts" ? "Finish" : "Continue"}
          </button>
        )}
      </div>

      <div className="mt-10 flex justify-center gap-4 text-stone-400">
        <Home className="h-4 w-4" />
        <User className="h-4 w-4" />
        <Zap className="h-4 w-4" />
        <Bell className="h-4 w-4" />
      </div>
    </div>
  );
}
