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
        <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-[#0B1F1A] text-emerald-300">
          <Settings className="h-5 w-5" />
        </div>
        <div>
          <h1 className="text-xl font-semibold">Settings</h1>
          <p className="text-xs text-stone-500">
            Channel flags live in notification_preferences. Profile fields live on users.
          </p>
        </div>
      </div>

      <div className="rounded-2xl border border-stone-200 bg-white p-5 space-y-3">
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
          className="rounded-xl bg-emerald-600 px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          Save settings
        </button>
        {msg && <p className="text-sm text-stone-600">{msg}</p>}
      </div>
    </div>
  );
}
