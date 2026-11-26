"use client";

/**
 * Device-local E2E for Meterly Support (human mode).
 *
 * P-256 ECDH + HKDF-SHA256 + AES-GCM. Private keys never leave this browser.
 * Agent chat is intentionally not E2E — Meterly processes those messages.
 */

const IDENTITY_PREFIX = "meterly.support.identity.v1:";
const CONV_PREFIX = "meterly.support.conv.v1:";

const te = new TextEncoder();

export type Identity = {
  publicKeyB64: string;
  privateJwk: JsonWebKey;
};

function b64(buf: ArrayBuffer | Uint8Array): string {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  let s = "";
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s);
}

function fromB64(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function asBuffer(u: Uint8Array): ArrayBuffer {
  const copy = new Uint8Array(u.byteLength);
  copy.set(u);
  return copy.buffer;
}

export function loadIdentity(token: string): Identity | null {
  try {
    const raw = localStorage.getItem(IDENTITY_PREFIX + token);
    if (!raw) return null;
    return JSON.parse(raw) as Identity;
  } catch {
    return null;
  }
}

export async function ensureIdentity(token: string): Promise<Identity> {
  const existing = loadIdentity(token);
  if (existing?.publicKeyB64 && existing.privateJwk) return existing;
  const pair = await crypto.subtle.generateKey(
    { name: "ECDH", namedCurve: "P-256" },
    true,
    ["deriveBits"]
  );
  const spki = await crypto.subtle.exportKey("spki", pair.publicKey);
  const jwk = await crypto.subtle.exportKey("jwk", pair.privateKey);
  const ident: Identity = { publicKeyB64: b64(spki), privateJwk: jwk };
  localStorage.setItem(IDENTITY_PREFIX + token, JSON.stringify(ident));
  return ident;
}

async function importPrivate(jwk: JsonWebKey): Promise<CryptoKey> {
  return crypto.subtle.importKey(
    "jwk",
    jwk,
    { name: "ECDH", namedCurve: "P-256" },
    false,
    ["deriveBits"]
  );
}

async function importPublicSPKI(b64spki: string): Promise<CryptoKey> {
  return crypto.subtle.importKey(
    "spki",
    asBuffer(fromB64(b64spki)),
    { name: "ECDH", namedCurve: "P-256" },
    false,
    []
  );
}

async function wrapKeyForPeer(
  privateJwk: JsonWebKey,
  peerPublicB64: string,
  threadId: string,
  convRaw: Uint8Array
): Promise<{ wrapped_key: string; nonce: string }> {
  const priv = await importPrivate(privateJwk);
  const pub = await importPublicSPKI(peerPublicB64);
  const bits = await crypto.subtle.deriveBits({ name: "ECDH", public: pub }, priv, 256);
  const ikm = await crypto.subtle.importKey("raw", bits, "HKDF", false, ["deriveKey"]);
  const aes = await crypto.subtle.deriveKey(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: te.encode("meterly-support-v1"),
      info: te.encode(threadId),
    },
    ikm,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt"]
  );
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, aes, asBuffer(convRaw));
  return { wrapped_key: b64(ct), nonce: b64(iv) };
}

async function unwrapKeyFromPeer(
  privateJwk: JsonWebKey,
  peerPublicB64: string,
  threadId: string,
  wrappedB64: string,
  nonceB64: string
): Promise<Uint8Array> {
  const priv = await importPrivate(privateJwk);
  const pub = await importPublicSPKI(peerPublicB64);
  const bits = await crypto.subtle.deriveBits({ name: "ECDH", public: pub }, priv, 256);
  const ikm = await crypto.subtle.importKey("raw", bits, "HKDF", false, ["deriveKey"]);
  const aes = await crypto.subtle.deriveKey(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: te.encode("meterly-support-v1"),
      info: te.encode(threadId),
    },
    ikm,
    { name: "AES-GCM", length: 256 },
    false,
    ["decrypt"]
  );
  const pt = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: asBuffer(fromB64(nonceB64)) },
    aes,
    asBuffer(fromB64(wrappedB64))
  );
  return new Uint8Array(pt);
}

export function loadConvKey(threadId: string): Uint8Array | null {
  try {
    const raw = localStorage.getItem(CONV_PREFIX + threadId);
    if (!raw) return null;
    return fromB64(raw);
  } catch {
    return null;
  }
}

export function saveConvKey(threadId: string, raw: Uint8Array) {
  localStorage.setItem(CONV_PREFIX + threadId, b64(raw));
}

export function newConvKey(): Uint8Array {
  return crypto.getRandomValues(new Uint8Array(32));
}

export async function wrapConversationKey(opts: {
  identity: Identity;
  threadId: string;
  convRaw: Uint8Array;
  holderUserId: string;
  holderPublicB64: string;
}): Promise<{ holder_user_id: string; wrapped_key: string; nonce: string }> {
  const w = await wrapKeyForPeer(
    opts.identity.privateJwk,
    opts.holderPublicB64,
    opts.threadId,
    opts.convRaw
  );
  return { holder_user_id: opts.holderUserId, ...w };
}

export async function unwrapConversationKey(opts: {
  identity: Identity;
  threadId: string;
  peerPublicB64: string;
  wrappedKeyB64: string;
  nonceB64: string;
}): Promise<Uint8Array> {
  return unwrapKeyFromPeer(
    opts.identity.privateJwk,
    opts.peerPublicB64,
    opts.threadId,
    opts.wrappedKeyB64,
    opts.nonceB64
  );
}

async function convAes(raw: Uint8Array, usage: KeyUsage[]): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", asBuffer(raw), { name: "AES-GCM" }, false, usage);
}

export async function encryptMessage(
  convRaw: Uint8Array,
  threadId: string,
  plaintext: string
): Promise<{ ciphertext: string; nonce: string }> {
  const aes = await convAes(convRaw, ["encrypt"]);
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ct = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv, additionalData: te.encode(threadId) },
    aes,
    te.encode(plaintext)
  );
  return { ciphertext: b64(ct), nonce: b64(iv) };
}

export async function decryptMessage(
  convRaw: Uint8Array,
  threadId: string,
  ciphertextB64: string,
  nonceB64: string
): Promise<string> {
  const aes = await convAes(convRaw, ["decrypt"]);
  const pt = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: asBuffer(fromB64(nonceB64)), additionalData: te.encode(threadId) },
    aes,
    asBuffer(fromB64(ciphertextB64))
  );
  return new TextDecoder().decode(pt);
}
