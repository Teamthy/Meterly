"use client";

import React, { useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/components/AuthContext";
import { apiFetch } from "@/lib/api";
import { createPasskey, CreationOptions, getPasskey, RequestOptions } from "@/lib/webauthn";
import { Fingerprint } from "lucide-react";

export default function SignInPage() {
  const { applySession, signOut, isPasskeySession, token } = useAuth();
  const router = useRouter();
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const origin = typeof window !== "undefined" ? window.location.origin : "";

  const login = async () => {
    setBusy(true);
    setErr(null);
    try {
      const opt = await apiFetch<RequestOptions>("/v1/auth/passkeys/login/begin", {
        method: "POST",
        body: { origin },
      });
      const assertion = await getPasskey(opt);
      const done = await apiFetch<{ token: string }>("/v1/auth/passkeys/login/finish", {
        method: "POST",
        body: { origin, ...assertion },
      });
      applySession(done.token);
      router.push("/");
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Passkey sign-in failed");
    } finally {
      setBusy(false);
    }
  };

  const register = async () => {
    setBusy(true);
    setErr(null);
    try {
      const opt = await apiFetch<CreationOptions>("/v1/auth/passkeys/register/begin", {
        method: "POST",
        token: isPasskeySession ? token : undefined,
        body: { display_name: name || undefined, origin },
      });
      const cred = await createPasskey(opt);
      const done = await apiFetch<{ token: string }>("/v1/auth/passkeys/register/finish", {
        method: "POST",
        body: { origin, name: "This device", ...cred },
      });
      applySession(done.token, name);
      router.push("/onboarding");
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Passkey registration failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto max-w-md rounded-2xl border border-stone-200 bg-white p-6">
      <div className="flex items-center gap-2 text-emerald-700">
        <Fingerprint className="h-5 w-5" />
        <p className="text-xs font-semibold uppercase tracking-wider">Authenticator</p>
      </div>
      <h1 className="mt-2 text-xl font-semibold">Device passkey</h1>
      <p className="mt-2 text-sm text-stone-600">
        Platform WebAuthn — Face ID, fingerprint or the device PIN. Meterly issues a session after the authenticator signs.
      </p>
      <label className="mt-6 block text-xs font-medium text-stone-600">
        Name for a new account
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="mt-1 w-full rounded-xl border border-stone-300 px-3 py-2 text-sm"
        />
      </label>
      {err && <p className="mt-3 text-sm text-rose-700">{err}</p>}
      <div className="mt-6 space-y-2">
        <button
          type="button"
          disabled={busy}
          onClick={login}
          className="w-full rounded-xl bg-[#0B1F1A] px-4 py-2.5 text-sm font-semibold text-white disabled:opacity-50"
        >
          Sign in with this device
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={register}
          className="w-full rounded-xl bg-emerald-600 px-4 py-2.5 text-sm font-semibold text-white disabled:opacity-50"
        >
          Create passkey &amp; account
        </button>
        {isPasskeySession && (
          <button type="button" onClick={() => signOut()} className="w-full text-xs text-stone-500">
            Sign out passkey session
          </button>
        )}
      </div>
    </div>
  );
}
