"use client";

import React, { useEffect, useState } from "react";
import Link from "next/link";
import { useAuth } from "@/components/AuthContext";
import { apiFetch } from "@/lib/api";
import { Passkey, UserProfile } from "@/lib/types";
import { Fingerprint, UserRound } from "lucide-react";

export default function ProfilePage() {
  const { token } = useAuth();
  const [profile, setProfile] = useState<UserProfile | null>(null);
  const [passkeys, setPasskeys] = useState<Passkey[]>([]);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [phone, setPhone] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  const load = async () => {
    const me = await apiFetch<{ profile: UserProfile }>("/v1/me", { token });
    setProfile(me.profile);
    setName(me.profile.display_name || "");
    try {
      const keys = await apiFetch<{ data: Passkey[] }>("/v1/me/passkeys", { token });
      setPasskeys(keys.data || []);
    } catch {
      setPasskeys([]);
    }
  };

  useEffect(() => {
    load().catch((e) => setMsg(e instanceof Error ? e.message : "Failed to load profile"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);

  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      const p = await apiFetch<UserProfile>("/v1/me", {
        method: "PATCH",
        token,
        body: {
          display_name: name,
          email: email || undefined,
          phone: phone || undefined,
        },
      });
      setProfile(p);
      setEmail("");
      setPhone("");
      setMsg("Saved to your user record.");
    } catch (e) {
      setMsg(e instanceof Error ? e.message : "Save failed");
    } finally {
      setBusy(false);
    }
  };

  const removeKey = async (id: string) => {
    await apiFetch(`/v1/me/passkeys/${id}`, { method: "DELETE", token });
    await load();
  };

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div className="flex items-center gap-3">
        <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-[#0B1F1A] text-emerald-300">
          <UserRound className="h-5 w-5" />
        </div>
        <div>
          <h1 className="text-xl font-semibold">Profile</h1>
          <p className="text-xs text-stone-500">Backed by the users table — encrypted email and phone, display name, onboarding.</p>
        </div>
      </div>

      <div className="rounded-2xl border border-stone-200 bg-white p-5 space-y-3">
        <label className="block text-xs font-medium text-stone-600">
          Display name
          <input value={name} onChange={(e) => setName(e.target.value)} className="mt-1 w-full rounded-xl border border-stone-300 px-3 py-2 text-sm" />
        </label>
        <p className="text-xs text-stone-500">
          Stored email: {profile?.email_masked || "—"} · phone: {profile?.phone_masked || "—"}
        </p>
        <label className="block text-xs font-medium text-stone-600">
          Replace email
          <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} className="mt-1 w-full rounded-xl border border-stone-300 px-3 py-2 text-sm" />
        </label>
        <label className="block text-xs font-medium text-stone-600">
          Replace phone (E.164)
          <input value={phone} onChange={(e) => setPhone(e.target.value)} className="mt-1 w-full rounded-xl border border-stone-300 px-3 py-2 text-sm" />
        </label>
        <button
          type="button"
          disabled={busy}
          onClick={save}
          className="rounded-xl bg-emerald-600 px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          Save profile
        </button>
        {msg && <p className="text-sm text-stone-600">{msg}</p>}
      </div>

      <div className="rounded-2xl border border-stone-200 bg-white p-5">
        <div className="flex items-center justify-between">
          <h2 className="flex items-center gap-2 text-sm font-semibold">
            <Fingerprint className="h-4 w-4" /> Passkeys
          </h2>
          <Link href="/signin" className="text-xs font-medium text-emerald-700">
            Add on this device
          </Link>
        </div>
        {passkeys.length === 0 ? (
          <p className="mt-3 text-sm text-stone-500">No device passkeys yet.</p>
        ) : (
          <ul className="mt-3 space-y-2">
            {passkeys.map((k) => (
              <li key={k.id} className="flex items-center justify-between rounded-xl border border-stone-200 px-3 py-2 text-sm">
                <span>
                  {k.name || "Unnamed device"}
                  <span className="ml-2 text-xs text-stone-400">last used {new Date(k.last_used_at).toLocaleString()}</span>
                </span>
                <button type="button" onClick={() => removeKey(k.id)} className="text-xs text-rose-700">
                  Remove
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
