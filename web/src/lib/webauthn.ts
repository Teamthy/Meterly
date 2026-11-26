function b64u(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let s = "";
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function b64uToBuf(s: string): ArrayBuffer {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

export type CreationOptions = {
  rp: { id: string; name: string };
  user: { id: string; name: string; displayName: string };
  challenge: string;
  pubKeyCredParams: Array<{ type: string; alg: number }>;
  timeout: number;
  attestation?: string;
  authenticatorSelection?: Record<string, unknown>;
  excludeCredentials?: Array<{ type: string; id: string }>;
};

export type RequestOptions = {
  challenge: string;
  timeout: number;
  rpId?: string;
  userVerification?: string;
};

export async function createPasskey(opt: CreationOptions): Promise<{
  id: string;
  rawId: string;
  type: string;
  clientDataJSON: string;
  authenticatorData: string;
  publicKey: string;
}> {
  if (!window.PublicKeyCredential) {
    throw new Error("This browser does not support device passkeys");
  }
  const cred = (await navigator.credentials.create({
    publicKey: {
      rp: opt.rp,
      user: {
        id: b64uToBuf(opt.user.id),
        name: opt.user.name,
        displayName: opt.user.displayName,
      },
      challenge: b64uToBuf(opt.challenge),
      pubKeyCredParams: opt.pubKeyCredParams as PublicKeyCredentialParameters[],
      timeout: opt.timeout,
      attestation: (opt.attestation as AttestationConveyancePreference) || "none",
      authenticatorSelection: opt.authenticatorSelection as AuthenticatorSelectionCriteria,
      excludeCredentials: (opt.excludeCredentials || []).map((c) => ({
        type: "public-key" as const,
        id: b64uToBuf(c.id),
      })),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("Passkey creation was cancelled");
  const att = cred.response as AuthenticatorAttestationResponse;
  const authData =
    typeof att.getAuthenticatorData === "function" ? att.getAuthenticatorData() : new ArrayBuffer(0);
  const pub =
    typeof att.getPublicKey === "function" && att.getPublicKey()
      ? att.getPublicKey()!
      : new ArrayBuffer(0);
  return {
    id: cred.id,
    rawId: b64u(cred.rawId),
    type: cred.type,
    clientDataJSON: b64u(att.clientDataJSON),
    authenticatorData: b64u(authData),
    publicKey: b64u(pub),
  };
}

export async function getPasskey(opt: RequestOptions): Promise<{
  id: string;
  rawId: string;
  authenticatorData: string;
  clientDataJSON: string;
  signature: string;
}> {
  if (!window.PublicKeyCredential) {
    throw new Error("This browser does not support device passkeys");
  }
  const cred = (await navigator.credentials.get({
    publicKey: {
      challenge: b64uToBuf(opt.challenge),
      timeout: opt.timeout,
      rpId: opt.rpId,
      userVerification: (opt.userVerification as UserVerificationRequirement) || "required",
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("Passkey sign-in was cancelled");
  const as = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: b64u(cred.rawId),
    authenticatorData: b64u(as.authenticatorData),
    clientDataJSON: b64u(as.clientDataJSON),
    signature: b64u(as.signature),
  };
}
