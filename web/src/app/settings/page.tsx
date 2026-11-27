"use client";

import React, { useEffect, useState } from "react";
import { useAuth } from "@/components/AuthContext";
import { apiFetch } from "@/lib/api";
import { NotificationPreferences, UserProfile } from "@/lib/types";
import { Settings } from "lucide-react";

export default function SettingsPage() {
  const { token } = useAuth();
  const [profile, setProfile] = useState<UserProfile | null>(null);
  const [sms, setSms] = useState(true);
  const [emailOn, setEmailOn] = useState(true);
  const [whatsapp, setWhatsapp] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  useEffect(() => {
    apiFetch<{ profile: UserProfile; notifications: NotificationPreferences }>("/v1/me/settings", { token })
      .then((r) => {
        setProfile(r.profile);
        setSms(r.notifications.sms);
        setEmailOn(r.notifications.email);
        setWhatsapp(r.notifications.whatsapp);
      })
      .catch((e) => setMsg(e instanceof Error ? e.message : "Failed to load settings"));
  }, [token]);

  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      await apiFetch("/v1/me/settings", {
        method: "PATCH",
        token,
        body: { sms, email_enabled: emailOn, whatsapp },
      });
      setMsg("Notification preferences saved.");
    } catch (e) {
      setMsg(e instanceof Error ? e.message : "Save failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <div className="flex items-center gap-3">
        <div className="flex h-10 w-10 items-center justify-center rounded-full border border-white/10 bg-white/5 text-volt">
          <Settings className="h-5 w-5" />
        </div>
        <div>
          <h1 className="font-display text-3xl font-semibold text-paper">Settings</h1>
          <p className="text-xs text-white/45">
            How we reach you. Profile lives on your user record.
          </p>
        </div>
      </div>

      <div className="ml-card space-y-3 p-5">
        <p className="text-sm text-stone-600">
          Signed in as <span className="font-medium">{profile?.display_name || profile?.user_id || "…"}</span>
          {profile?.onboarding_step && profile.onboarding_step !== "done" ? (
            <span className="ml-2 text-xs text-amber-700">onboarding: {profile.onboarding_step}</span>
          ) : null}
        </p>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={sms} onChange={(e) => setSms(e.target.checked)} />
          SMS
        </label>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={emailOn} onChange={(e) => setEmailOn(e.target.checked)} />
          Email
        </label>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={whatsapp} onChange={(e) => setWhatsapp(e.target.checked)} />
          WhatsApp
        </label>
        <button
          type="button"
          disabled={busy}
          onClick={save}
          className="rounded-full bg-ink px-4 py-2 text-sm font-semibold text-volt disabled:opacity-50"
        >
          Save settings
        </button>
        {msg && <p className="text-sm text-stone-600">{msg}</p>}
      </div>
    </div>
  );
}
